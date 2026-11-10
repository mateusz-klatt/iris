package gitinfo

// IndexID names one generation of an index file (ADR 018): its device and inode, size,
// modification and change times, and its last 32 bytes. A published index ends with a checksum
// of its content, so two generations with different content differ in Tail even when the five
// fields are equal. The hook reads it before the first read of a reduced snapshot and after the
// last; equal means both reads saw one generation. It is read only where a reduced snapshot is
// taken, which is not Windows.
//
// What it cannot tell: with index.skipHash the checksum is zeros, and only the five fields
// remain. A replacement that restores all five within one clock tick is then not seen, and
// neither is an in-place rewrite within one tick that keeps the size.
type IndexID struct {
	Device, Inode     uint64
	Size              int64
	Modified, Changed int64
	Tail              [32]byte
}
