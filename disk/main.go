// Command disk keeps a node's disks from filling. It runs on every node of the
// cluster (infra/aws/k8s/disk.yaml) and climbs one ladder per watched
// filesystem, every minute:
//
//	75%  evict caches: unused images, the journal past 256M, /tmp files and build
//	     caches nothing has touched for a day
//	80%  label the node hanzo.ai/disk=pressure, which every disk-heavy pod (builds,
//	     CI runners, sandboxes) carries a NotIn affinity against; cleared below 75%
//	85%  purge: the journal past 64M, rotated logs, the forge's push quarantines
//	     older than ten minutes; then, still at 85%, grow the EBS volume by half
//	     (to at most 4 TiB, once per six hours) and the filesystem with it
//	90%  cordon the node; lifted below 80%. o11y pages on the same reading
//	     (NodeDiskCritical)
//
// A node's disks are a cache of what lives in object storage, so none of this
// touches a volume's data: it removes what the node can download or rebuild,
// stops taking more, and buys room.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

const (
	every      = time.Minute
	evictEvery = 10 * time.Minute // the cheapest rung still walks directories
	purgeEvery = 5 * time.Minute
)

type guard struct {
	log  *slog.Logger
	name string
	kube *kube
	ebs  *ebs

	evicted, purged time.Time
	reported        float64
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	g := &guard{log: log, name: os.Getenv("NODE_NAME")}
	if g.name == "" {
		log.Error("NODE_NAME is unset")
		os.Exit(1)
	}
	k, err := inCluster()
	if err != nil {
		log.Error("cluster", "err", err)
		os.Exit(1)
	}
	g.kube = k
	g.ebs = &ebs{c: &http.Client{Timeout: 30 * time.Second}}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if r, err := g.ebs.meta(ctx, "/latest/meta-data/placement/region"); err == nil {
		g.ebs.region = r
	} else {
		log.Warn("no instance metadata: volumes will not grow", "err", err)
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		g.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (g *guard) read() ([]mount, float64, error) {
	f, err := os.Open("/proc/1/mountinfo")
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	ms, err := mounts(f)
	if err != nil {
		return nil, 0, err
	}
	top := 0.0
	for i := range ms {
		if err := measure(&ms[i]); err != nil {
			return nil, 0, fmt.Errorf("%s: %w", ms[i].Point, err)
		}
		top = max(top, ms[i].Fill)
	}
	return ms, top, nil
}

func (g *guard) tick(ctx context.Context) {
	ms, top, err := g.read()
	if err != nil {
		g.log.Error("measure", "err", err)
		return
	}
	now := time.Now()
	if top >= evictAt && now.Sub(g.evicted) >= evictEvery {
		g.report(ms, "evict")
		evict(ctx, g.log)
		g.evicted = now
		ms, top, _ = g.read()
	}
	if top >= purgeAt && now.Sub(g.purged) >= purgeEvery {
		g.report(ms, "purge")
		purge(ctx, g.log)
		g.purged = now
		ms, top, _ = g.read()
	}
	for _, m := range ms {
		if m.Fill >= purgeAt {
			g.grow(ctx, m)
		}
		g.fit(ctx, m)
	}
	if top-g.reported >= 0.01 || g.reported-top >= 0.01 {
		g.report(ms, "")
	}
	g.place(ctx, top)
}

func (g *guard) report(ms []mount, why string) {
	attrs := []any{"why", why}
	top := 0.0
	for _, m := range ms {
		attrs = append(attrs, m.Point, fmt.Sprintf("%.1f%% of %s", 100*m.Fill, gib(m.Size)))
		top = max(top, m.Fill)
	}
	g.reported = top
	g.log.Info("disk", attrs...)
}

// place sets the node's label and cordon from the fullest watched filesystem.
func (g *guard) place(ctx context.Context, top float64) {
	n, err := g.kube.node(ctx, g.name)
	if err != nil {
		g.log.Error("node", "err", err)
		return
	}
	labels, annotations := map[string]any{}, map[string]any{}
	patch := map[string]any{}
	if want := pressured(top, n.Pressure); want != n.Pressure {
		if want {
			labels[Label] = Pressure
		} else {
			labels[Label] = nil
		}
	}
	switch want := cordoned(top, n.OurCordon); {
	case want && !n.OurCordon && !n.Cordoned:
		patch["spec"] = map[string]any{"unschedulable": true}
		annotations[Cordoned] = "true"
	case !want && n.OurCordon:
		if n.Cordoned {
			patch["spec"] = map[string]any{"unschedulable": false}
		}
		annotations[Cordoned] = nil
	}
	if len(labels)+len(annotations)+len(patch) == 0 {
		return
	}
	meta := map[string]any{}
	if len(labels) > 0 {
		meta["labels"] = labels
	}
	if len(annotations) > 0 {
		meta["annotations"] = annotations
	}
	if len(meta) > 0 {
		patch["metadata"] = meta
	}
	if err := g.kube.patch(ctx, g.name, patch); err != nil {
		g.log.Error("node patch", "err", err)
		return
	}
	g.log.Info("node", "fill", fmt.Sprintf("%.1f%%", 100*top), "labels", labels, "spec", patch["spec"], "annotations", annotations)
}

// grow asks AWS for a larger volume under m, once per cooldown.
func (g *guard) grow(ctx context.Context, m mount) {
	if g.ebs.region == "" {
		return
	}
	b, err := device(host, m.Dev)
	if err != nil || b.Volume == "" {
		g.log.Warn("grow: no EBS volume under "+m.Point, "err", err)
		return
	}
	size, err := g.ebs.size(ctx, b.Volume)
	if err != nil {
		g.log.Error("grow", "volume", b.Volume, "err", err)
		return
	}
	to, ok := grown(size)
	if !ok {
		g.log.Warn("grow: volume at its ceiling", "volume", b.Volume, "gib", size)
		return
	}
	at, state, err := g.ebs.changed(ctx, b.Volume)
	if err != nil {
		g.log.Error("grow", "volume", b.Volume, "err", err)
		return
	}
	if since := time.Since(at); since < cooldown || state == "modifying" {
		g.log.Warn("grow: volume changed recently", "volume", b.Volume, "state", state, "since", since.Round(time.Minute).String())
		return
	}
	if err := g.ebs.grow(ctx, b.Volume, to); err != nil {
		g.log.Error("grow", "volume", b.Volume, "err", err)
		return
	}
	g.log.Warn("grow", "node", g.name, "mount", m.Point, "volume", b.Volume, "from_gib", size, "to_gib", to, "fill", fmt.Sprintf("%.1f%%", 100*m.Fill))
}

// fit grows the partition and the filesystem under m to the device, once the
// device is larger than they are: after a grow, whoever requested it.
func (g *guard) fit(ctx context.Context, m mount) {
	b, err := device(host, m.Dev)
	if err != nil {
		return
	}
	const slack = 128 << 20
	if b.Part > 0 && b.Room > slack {
		out, err := run(ctx, "/usr/bin/growpart", "/dev/"+b.Disk, fmt.Sprint(b.Part))
		g.log.Warn("growpart", "dev", b.Name, "err", err, "out", last(out))
		if b, err = device(host, m.Dev); err != nil {
			return
		}
	}
	size, err := fsBytes(filepath.Join(host, "dev", b.Name), m.Type)
	if err != nil || size+slack >= b.Bytes {
		return
	}
	var argv []string
	switch m.Type {
	case "xfs":
		argv = []string{"/usr/sbin/xfs_growfs", m.Point}
	default:
		argv = []string{"/usr/sbin/resize2fs", "/dev/" + b.Name}
	}
	out, err := run(ctx, argv...)
	g.log.Warn("resize", "mount", m.Point, "dev", b.Name, "from", gib(size), "to", gib(b.Bytes), "err", err, "out", last(out))
}

func gib(b uint64) string { return fmt.Sprintf("%.0fG", float64(b)/(1<<30)) }
