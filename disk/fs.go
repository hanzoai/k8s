package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// host is the node's root as this process sees it. With hostPID, PID 1 is the
// node's init, and its root is the node's filesystem with every mount on it —
// no hostPath volume, and nothing of the node mounted into this container.
const host = "/proc/1/root"

// watched are the mount points this program keeps from filling: the root, which
// holds images, logs and the kubelet, and the disk k3s keeps local volumes on
// where a node has one.
var watched = []string{"/", "/var/lib/rancher/k3s/storage"}

// mount is one watched filesystem.
type mount struct {
	Point string // where it is mounted on the node
	Dev   string // major:minor of its block device
	Type  string // ext4, xfs
	Fill  float64
	Size  uint64 // bytes the filesystem reports, for logs
	Avail uint64
}

// mounts reads the node's mountinfo (/proc/1/mountinfo) and returns the
// watched mount points that exist, one per block device: a bind of the same
// device under a second name is the same disk and is counted once.
func mounts(mountinfo io.Reader) ([]mount, error) {
	found := map[string]mount{}
	sc := bufio.NewScanner(mountinfo)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		sep := -1
		for i, s := range f {
			if s == "-" {
				sep = i
				break
			}
		}
		if sep < 6 || sep+1 >= len(f) {
			continue
		}
		point := f[4]
		for _, w := range watched {
			if point == w {
				// Later lines mount over earlier ones; the last is what a path resolves to.
				found[w] = mount{Point: w, Dev: f[2], Type: f[sep+1]}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	var out []mount
	seen := map[string]bool{}
	for _, w := range watched {
		m, ok := found[w]
		if !ok || seen[m.Dev] {
			continue
		}
		seen[m.Dev] = true
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, errors.New("no watched filesystem in mountinfo")
	}
	return out, nil
}

// measure fills in how full m is, the way df reports it: used over what an
// unprivileged writer could ever use (used + available), so the reserved blocks
// count as full.
func measure(m *mount) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(host+m.Point, &st); err != nil {
		return err
	}
	bs := uint64(st.Bsize)
	used := (st.Blocks - st.Bfree) * bs
	m.Avail = st.Bavail * bs
	m.Size = st.Blocks * bs
	if used+m.Avail == 0 {
		return fmt.Errorf("%s reports no blocks", m.Point)
	}
	m.Fill = float64(used) / float64(used+m.Avail)
	return nil
}

// block is the device under a filesystem, read from sysfs.
type block struct {
	Name   string // nvme0n1p1, the node /dev/<Name>
	Disk   string // nvme0n1, the whole disk the volume is
	Part   int    // partition number, 0 for a whole disk
	Bytes  uint64 // size of Name
	Room   uint64 // bytes of Disk after the end of Name
	Volume string // EBS volume id, from the NVMe serial; empty off EBS
}

// device resolves major:minor through sysfs (under root, normally host).
func device(root, dev string) (block, error) {
	link := filepath.Join(root, "sys/dev/block", dev)
	target, err := filepath.EvalSymlinks(link)
	if err != nil {
		return block{}, err
	}
	b := block{Name: filepath.Base(target), Disk: filepath.Base(target)}
	sectors, err := readUint(filepath.Join(target, "size"))
	if err != nil {
		return block{}, err
	}
	b.Bytes = sectors * 512
	if p, err := readUint(filepath.Join(target, "partition")); err == nil {
		b.Part = int(p)
		b.Disk = filepath.Base(filepath.Dir(target))
		start, err := readUint(filepath.Join(target, "start"))
		if err != nil {
			return block{}, err
		}
		disk, err := readUint(filepath.Join(filepath.Dir(target), "size"))
		if err != nil {
			return block{}, err
		}
		if end := start + sectors; disk > end {
			b.Room = (disk - end) * 512
		}
	}
	// An EBS volume on a Nitro instance is an NVMe namespace whose serial is the
	// volume id without its dash: vol0123… for vol-0123….
	if s, err := os.ReadFile(filepath.Join(root, "sys/block", b.Disk, "device/serial")); err == nil {
		if v := strings.TrimSpace(string(s)); strings.HasPrefix(v, "vol") && !strings.HasPrefix(v, "vol-") {
			b.Volume = "vol-" + strings.TrimPrefix(v, "vol")
		}
	}
	return b, nil
}

func readUint(path string) (uint64, error) {
	s, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(s)), 10, 64)
}

// fsBytes is the size the filesystem on dev records in its own superblock —
// not statfs, which subtracts ext4's metadata and so always reads a few percent
// under the device. Equal to the device once the filesystem fills it.
func fsBytes(dev, kind string) (uint64, error) {
	f, err := os.Open(dev)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	sb := make([]byte, 2048)
	if _, err := io.ReadFull(f, sb); err != nil {
		return 0, err
	}
	return superblock(sb, kind)
}

func superblock(sb []byte, kind string) (uint64, error) {
	switch kind {
	case "ext4", "ext3", "ext2":
		s := sb[1024:]
		if binary.LittleEndian.Uint16(s[0x38:]) != 0xEF53 {
			return 0, errors.New("no ext superblock")
		}
		blocks := uint64(binary.LittleEndian.Uint32(s[0x04:]))
		if binary.LittleEndian.Uint32(s[0x60:])&0x80 != 0 { // INCOMPAT_64BIT
			blocks |= uint64(binary.LittleEndian.Uint32(s[0x150:])) << 32
		}
		return blocks << (10 + binary.LittleEndian.Uint32(s[0x18:])), nil
	case "xfs":
		if string(sb[:4]) != "XFSB" {
			return 0, errors.New("no xfs superblock")
		}
		return binary.BigEndian.Uint64(sb[8:]) * uint64(binary.BigEndian.Uint32(sb[4:])), nil
	}
	return 0, fmt.Errorf("cannot read the size of a %s filesystem", kind)
}
