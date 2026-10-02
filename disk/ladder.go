package main

import "math"

// The ladder: what a node does as a watched filesystem fills. A node's disks are
// a cache of things that live elsewhere, so every rung below the last one gives
// space back or stops taking more; only the last one asks a person.
const (
	evictAt  = 0.75 // drop caches: unused images, journal, stale /tmp and build caches
	admitAt  = 0.80 // stop admitting disk-heavy pods: label the node hanzo.ai/disk=pressure
	purgeAt  = 0.85 // emergency purge, then grow the volume
	cordonAt = 0.90 // cordon the node; o11y pages on the same reading
)

// Label is the node label disk-heavy pods carry a NotIn affinity against.
const (
	Label    = "hanzo.ai/disk"
	Pressure = "pressure"
	// Cordoned marks a cordon this program set, so it never lifts one a person
	// set for another reason.
	Cordoned = "hanzo.ai/disk-cordon"
)

// pressured is whether the node should carry Label=Pressure at fill f, given
// whether it carries it now. It is set at admitAt and cleared below evictAt; in
// between the node keeps what it has, so a disk hovering at 78% does not flip
// the label every minute.
func pressured(f float64, now bool) bool {
	switch {
	case f >= admitAt:
		return true
	case f < evictAt:
		return false
	}
	return now
}

// cordoned is whether this program's cordon should hold at fill f, given whether
// it holds now. Set at cordonAt, lifted below admitAt.
func cordoned(f float64, now bool) bool {
	switch {
	case f >= cordonAt:
		return true
	case f < admitAt:
		return false
	}
	return now
}

// Growth bounds. AWS allows one modification per volume every six hours; a grow
// adds half the volume, and none is taken past maxGiB.
const (
	maxGiB   = 4096
	growRate = 1.5
)

// grown is the size, in GiB, a volume of size GiB grows to, and whether there is
// any room left to grow at all.
func grown(size int) (int, bool) {
	if size >= maxGiB {
		return size, false
	}
	n := int(math.Ceil(float64(size) * growRate))
	if n > maxGiB {
		n = maxGiB
	}
	return n, n > size
}
