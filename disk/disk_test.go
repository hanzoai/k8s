package main

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The label holds between 75% and 80% whichever way the disk is moving, so a
// disk hovering in that band never flips it.
func TestPressureHasHysteresis(t *testing.T) {
	cases := []struct {
		fill      float64
		now, want bool
	}{
		{0.50, false, false}, {0.50, true, false},
		{0.749, true, false},
		{0.75, false, false}, {0.75, true, true},
		{0.79, false, false}, {0.79, true, true},
		{0.80, false, true}, {0.95, false, true},
	}
	for _, c := range cases {
		if got := pressured(c.fill, c.now); got != c.want {
			t.Errorf("pressured(%.3f, %v) = %v, want %v", c.fill, c.now, got, c.want)
		}
	}
}

func TestCordonHasHysteresis(t *testing.T) {
	cases := []struct {
		fill      float64
		now, want bool
	}{
		{0.89, false, false}, {0.90, false, true},
		{0.85, true, true}, {0.80, true, true},
		{0.799, true, false}, {0.70, false, false},
	}
	for _, c := range cases {
		if got := cordoned(c.fill, c.now); got != c.want {
			t.Errorf("cordoned(%.3f, %v) = %v, want %v", c.fill, c.now, got, c.want)
		}
	}
}

func TestGrowthAddsHalfUpToTheCeiling(t *testing.T) {
	cases := []struct {
		size, want int
		ok         bool
	}{
		{60, 90, true}, {1, 2, true}, {1000, 1500, true}, {1601, 2402, true},
		{3000, 4096, true}, {4095, 4096, true}, {4096, 4096, false}, {5000, 5000, false},
	}
	for _, c := range cases {
		got, ok := grown(c.size)
		if got != c.want || ok != c.ok {
			t.Errorf("grown(%d) = %d, %v; want %d, %v", c.size, got, ok, c.want, c.ok)
		}
	}
}

// One entry per block device, the last mount at a point wins, and a point that
// is not a mount is not watched.
func TestMountsWatchesEachDiskOnce(t *testing.T) {
	info := strings.Join([]string{
		"24 1 259:1 / / rw,relatime shared:1 - ext4 /dev/root rw,discard",
		"40 24 0:22 / /proc rw - proc proc rw",
		"91 24 259:5 / /mnt/hd rw shared:40 - ext4 /dev/nvme1n1 rw",
		"92 24 259:5 /k3s /var/lib/rancher/k3s/storage rw shared:40 - ext4 /dev/nvme1n1 rw",
	}, "\n")
	ms, err := mounts(strings.NewReader(info))
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 || ms[0].Point != "/" || ms[0].Dev != "259:1" || ms[1].Point != "/var/lib/rancher/k3s/storage" || ms[1].Type != "ext4" {
		t.Fatalf("mounts = %+v", ms)
	}
	// The storage directory on the root disk is the root disk.
	ms, _ = mounts(strings.NewReader("24 1 259:1 / / rw - ext4 /dev/root rw\n25 24 259:1 /x /var/lib/rancher/k3s/storage rw - ext4 /dev/root rw\n"))
	if len(ms) != 1 {
		t.Fatalf("a bind of the root disk counted twice: %+v", ms)
	}
	if _, err := mounts(strings.NewReader("40 24 0:22 / /proc rw - proc proc rw\n")); err == nil {
		t.Fatal("no watched mount is an error, not an empty answer")
	}
}

// A partition resolves to its disk, the room after it, and the EBS volume named
// by the disk's NVMe serial.
func TestDeviceReadsSysfs(t *testing.T) {
	root := t.TempDir()
	disk := filepath.Join(root, "sys/devices/pci0000:00/nvme/nvme0/nvme0n1")
	part := filepath.Join(disk, "nvme0n1p1")
	write(t, filepath.Join(disk, "size"), "2097152000") // 1000 GiB
	write(t, filepath.Join(part, "size"), "125829120")  // 60 GiB
	write(t, filepath.Join(part, "start"), "2048")
	write(t, filepath.Join(part, "partition"), "1")
	write(t, filepath.Join(root, "sys/block/nvme0n1/device/serial"), "vol0c45cf5a13bfd27e4 \n")
	link(t, part, filepath.Join(root, "sys/dev/block/259:1"))

	b, err := device(root, "259:1")
	if err != nil {
		t.Fatal(err)
	}
	if b.Name != "nvme0n1p1" || b.Disk != "nvme0n1" || b.Part != 1 || b.Bytes != 60<<30 || b.Volume != "vol-0c45cf5a13bfd27e4" {
		t.Fatalf("device = %+v", b)
	}
	if want := uint64(2097152000-125829120-2048) * 512; b.Room != want {
		t.Fatalf("room = %d, want %d", b.Room, want)
	}

	whole := filepath.Join(root, "sys/devices/pci0000:00/nvme/nvme1/nvme1n1")
	write(t, filepath.Join(whole, "size"), "3355443200")
	link(t, whole, filepath.Join(root, "sys/dev/block/259:5"))
	if b, err = device(root, "259:5"); err != nil || b.Part != 0 || b.Disk != "nvme1n1" || b.Room != 0 || b.Volume != "" {
		t.Fatalf("whole disk = %+v, %v", b, err)
	}
}

func TestSuperblockSize(t *testing.T) {
	ext := make([]byte, 2048)
	s := ext[1024:]
	binary.LittleEndian.PutUint16(s[0x38:], 0xEF53)
	binary.LittleEndian.PutUint32(s[0x18:], 2) // 4 KiB blocks
	binary.LittleEndian.PutUint32(s[0x04:], 0x10)
	binary.LittleEndian.PutUint32(s[0x60:], 0x80) // 64bit
	binary.LittleEndian.PutUint32(s[0x150:], 1)
	if got, err := superblock(ext, "ext4"); err != nil || got != (1<<32|0x10)*4096 {
		t.Fatalf("ext4 = %d, %v", got, err)
	}
	xfs := make([]byte, 2048)
	copy(xfs, "XFSB")
	binary.BigEndian.PutUint32(xfs[4:], 4096)
	binary.BigEndian.PutUint64(xfs[8:], 1000)
	if got, err := superblock(xfs, "xfs"); err != nil || got != 4096000 {
		t.Fatalf("xfs = %d, %v", got, err)
	}
	if _, err := superblock(make([]byte, 2048), "ext4"); err == nil {
		t.Fatal("a missing magic read as a size")
	}
}

func TestRotatedLogs(t *testing.T) {
	for name, want := range map[string]bool{
		"syslog.1": true, "kern.log.2.gz": true, "0.log.20261002-120102.gz": true, "dpkg.log.old": true,
		"syslog": false, "kern.log": false, "0.log.20261002-120102": false, "cloud-init.log": false,
	} {
		if got := rotated("", entry{name}); got != want {
			t.Errorf("rotated(%q) = %v, want %v", name, got, want)
		}
	}
}

// A build cache is found by its tag and kept while anything inside is fresh.
func TestCachesAreTaggedAndIdle(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-48 * time.Hour)
	stale := filepath.Join(root, "home/ubuntu/engine/target")
	fresh := filepath.Join(root, "home/ubuntu/ml/target")
	gobuild := filepath.Join(root, "root/.cache/go-build")
	plain := filepath.Join(root, "home/ubuntu/src")
	for _, d := range []string{stale, fresh} {
		write(t, filepath.Join(d, "CACHEDIR.TAG"), "Signature: 8a477f597d28d172789f06886806bc55\n")
		write(t, filepath.Join(d, "debug/x"), "x")
	}
	write(t, filepath.Join(gobuild, "00/a"), "x")
	write(t, filepath.Join(plain, "main.go"), "package main")
	for _, d := range []string{stale, gobuild, plain} {
		age(t, d, old)
	}
	age(t, filepath.Join(fresh, "CACHEDIR.TAG"), old)

	got := caches(root)
	if len(got) != 2 || got[0] != gobuild && got[1] != gobuild || !contains(got, stale) {
		t.Fatalf("caches = %v, want %s and %s", got, stale, gobuild)
	}
}

func TestQuarantinesAreIdleOnly(t *testing.T) {
	root := t.TempDir()
	repos := filepath.Join(root, "var/lib/rancher/k3s/storage/pvc-1_hanzo_hanzo-git-data/git/data/repositories")
	idle := filepath.Join(repos, "hanzoai/team.git/objects/tmp_objdir-incoming-a")
	busy := filepath.Join(repos, "hanzoai/ui.git/objects/tmp_objdir-incoming-b")
	write(t, filepath.Join(idle, "pack/p"), "x")
	write(t, filepath.Join(busy, "pack/p"), "x")
	age(t, idle, time.Now().Add(-time.Hour))
	age(t, filepath.Join(busy, "pack"), time.Now().Add(-time.Hour))
	got := quarantines(root, 10*time.Minute)
	if len(got) != 1 || got[0] != idle {
		t.Fatalf("quarantines = %v, want only %s", got, idle)
	}
}

// AWS's published derivation example (Signature Version 4, "deriving a signing key").
func TestSigningKey(t *testing.T) {
	got := hex.EncodeToString(key("wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY", "20120215", "us-east-1", "iam"))
	if got != "f4780e2d9f65fa895f9c67b32ce1baf0b0d8a43505a000a1a9e090d414db404d" {
		t.Fatalf("signing key = %s", got)
	}
}

func TestModificationsDecode(t *testing.T) {
	var out struct {
		Mods []struct {
			State string    `xml:"modificationState"`
			Start time.Time `xml:"startTime"`
		} `xml:"volumeModificationSet>item"`
	}
	body := `<DescribeVolumesModificationsResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><volumeModificationSet><item><volumeId>vol-1</volumeId><modificationState>optimizing</modificationState><startTime>2026-10-02T09:10:11.000Z</startTime></item></volumeModificationSet></DescribeVolumesModificationsResponse>`
	if err := xml.Unmarshal([]byte(body), &out); err != nil || len(out.Mods) != 1 || out.Mods[0].State != "optimizing" || out.Mods[0].Start.Hour() != 9 {
		t.Fatalf("decoded %+v, %v", out, err)
	}
}

type entry struct{ name string }

func (e entry) Name() string               { return e.name }
func (e entry) IsDir() bool                { return false }
func (e entry) Type() fs.FileMode          { return 0 }
func (e entry) Info() (fs.FileInfo, error) { return nil, nil }

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func link(t *testing.T, target, at string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, at); err != nil {
		t.Fatal(err)
	}
}

// age sets the time of path and everything under it.
func age(t *testing.T, path string, when time.Time) {
	t.Helper()
	filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err == nil {
			os.Chtimes(p, when, when)
		}
		return nil
	})
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
