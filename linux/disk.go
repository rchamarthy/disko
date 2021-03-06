package linux

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"unicode/utf16"

	"github.com/anuvu/disko"
	"github.com/anuvu/disko/partid"
	"github.com/rekby/gpt"
	"github.com/rekby/mbr"
	"golang.org/x/sys/unix"
)

const (
	sectorSize512 = 512
	sectorSize4k  = 4096
)

// ErrNoPartitionTable is returned if there is no partition table.
var ErrNoPartitionTable error = errors.New("no Partition Table Found")

// toGPTPartition - convert the Partition type into a gpt.Partition
func toGPTPartition(p disko.Partition, sectorSize uint) gpt.Partition {
	return gpt.Partition{
		Type:          gpt.PartType(p.Type),
		Id:            gpt.Guid(p.ID),
		FirstLBA:      Floor(p.Start, uint64(sectorSize)) / uint64(sectorSize),
		LastLBA:       Floor(p.Last, uint64(sectorSize)) / uint64(sectorSize),
		Flags:         gpt.Flags{},
		PartNameUTF16: getPartName(p.Name),
		TrailingBytes: []byte{},
	}
}

// getDiskType(udInfo) return the diskType for the disk represented
//   by the udev info provided.  Supports a block device
func getDiskType(udInfo disko.UdevInfo) (disko.DiskType, error) {
	var kname = udInfo.Name

	if strings.HasPrefix(kname, "nvme") {
		return disko.NVME, nil
	}

	if isKvm() {
		psuedoSsd := regexp.MustCompile("^ssd[0-9-]")
		if psuedoSsd.MatchString(udInfo.Properties["ID_SERIAL"]) {
			return disko.SSD, nil
		}
	}

	bd, err := getPartitionsBlockDevice(path.Join("/dev", kname))
	if err != nil {
		return disko.HDD, nil
	}

	syspath, err := getSysPathForBlockDevicePath(bd)
	if err != nil {
		return disko.HDD, nil
	}

	content, err := ioutil.ReadFile(
		fmt.Sprintf("%s/%s", syspath, "queue/rotational"))
	if err != nil {
		return disko.HDD,
			fmt.Errorf("failed to read %s/queue/rotational for %s", syspath, kname)
	}

	if string(content) == "0\n" {
		return disko.SSD, nil
	}

	return disko.HDD, nil
}

func getAttachType(udInfo disko.UdevInfo) disko.AttachmentType {
	bus := udInfo.Properties["ID_BUS"]
	attach := disko.UnknownAttach

	switch bus {
	case "ata":
		attach = disko.ATA
	case "usb":
		attach = disko.USB
	case "scsi":
		attach = disko.SCSI
	case "virtio":
		attach = disko.VIRTIO
	case "":
		if strings.Contains(udInfo.SysPath, "/virtio") {
			attach = disko.VIRTIO
		} else if strings.Contains(udInfo.SysPath, "/nvme/") {
			attach = disko.PCIE
		}
	}

	return attach
}

func readTableSearch(fp io.ReadSeeker, sizes []uint) (gpt.Table, uint, error) {
	const noGptFound = "Bad GPT signature"
	var gptTable gpt.Table
	var err error
	var size uint

	for _, size = range sizes {
		// consider seek failure to be fatal
		if _, err := fp.Seek(int64(size), io.SeekStart); err != nil {
			return gpt.Table{}, size, err
		}

		if gptTable, err = gpt.ReadTable(fp, uint64(size)); err != nil {
			if err.Error() == noGptFound {
				continue
			}

			return gpt.Table{}, size, err
		}

		return gptTable, size, nil
	}

	return gpt.Table{}, size, ErrNoPartitionTable
}

func readTable(fp io.ReadSeeker) (gpt.Table, uint, error) {
	return readTableSearch(fp, []uint{sectorSize512, sectorSize4k})
}

func findPartitions(fp io.ReadSeeker) (disko.PartitionSet, uint, error) {
	var err error
	var ssize uint
	var gptTable gpt.Table

	parts := disko.PartitionSet{}

	gptTable, ssize, err = readTable(fp)
	if err != nil {
		return parts, ssize, ErrNoPartitionTable
	}

	ssize64 := uint64(ssize)

	for n, p := range gptTable.Partitions {
		if p.IsEmpty() {
			continue
		}

		part := disko.Partition{
			Start:  p.FirstLBA * ssize64,
			Last:   p.LastLBA*ssize64 + ssize64 - 1,
			ID:     disko.GUID(p.Id),
			Type:   disko.PartType(p.Type),
			Name:   p.Name(),
			Number: uint(n + 1),
		}
		parts[part.Number] = part
	}

	return parts, ssize, nil
}

func getDiskNames() ([]string, error) {
	realDiskKnameRegex := regexp.MustCompile("^((s|v|xv|h)d[a-z]|nvme[0-9]n[0-9]+)$")
	disks := []string{}

	files, err := ioutil.ReadDir("/sys/block")
	if err != nil {
		return []string{}, err
	}

	for _, file := range files {
		if realDiskKnameRegex.MatchString(file.Name()) {
			disks = append(disks, file.Name())
		}
	}

	return disks, nil
}

func getPathForKname(kname string) string {
	return path.Join("/dev", kname)
}

func getKnameAndPathForBlockDevice(nameOrPath string) (string, string, error) {
	syspath, err := getSysPathForBlockDevicePath(nameOrPath)
	if err != nil {
		return "", "", err
	}

	kname := path.Base(syspath)

	return kname, getPathForKname(kname), nil
}

func getKnameForBlockDevicePath(dev string) (string, error) {
	// given '/dev/sda1' (or any valid block device path) return 'sda'
	kname, err := getSysPathForBlockDevicePath(dev)
	if err != nil {
		return "", err
	}

	return path.Base(kname), nil
}

func getSysPathForBlockDevicePath(dev string) (string, error) {
	// Return the path in /sys/class/block/<device> for a given
	// block device kname or path.
	var syspath string
	var sysdir string = "/sys/class/block"

	if strings.Contains(dev, "/") {
		// after symlink resolution, devpath = '/dev/sda' or '/dev/sdb1'
		// no longer something like /dev/disk/by-id/foo
		devpath, err := filepath.EvalSymlinks(dev)
		if err != nil {
			return "", err
		}

		syspath = fmt.Sprintf("%s/%s", sysdir, path.Base(devpath))
	} else {
		// assume this is 'sda', something that would be in /sys/class/block
		syspath = fmt.Sprintf("%s/%s", sysdir, dev)
	}

	_, err := os.Stat(syspath)
	if err != nil {
		return "", err
	}

	return syspath, nil
}

func getPartitionsBlockDevice(dev string) (string, error) {
	// return the block device name ('sda') given input
	// of 'sda1', /dev/sda1, or /dev/sda
	syspath, err := getSysPathForBlockDevicePath(dev)
	if err != nil {
		return "", err
	}

	_, err = ioutil.ReadFile(fmt.Sprintf("%s/%s", syspath, "partition"))
	if err != nil {
		// dev is a block device, there is no /sys/class/block/<dev>/partition
		return path.Base(syspath), nil
	}

	// evalSymlinks on a partition will return
	// /sys/devices/<bus>/<path>/<compoents>/block/<diskName>/<PartitionName>
	sysfull, err := filepath.EvalSymlinks(syspath)
	if err != nil {
		return "", err
	}

	return path.Base(path.Dir(sysfull)), nil
}

func getPartName(s string) [72]byte {
	codes := utf16.Encode([]rune(s))
	b := [72]byte{}

	for i, r := range codes {
		b[i*2] = byte(r)
		b[i*2+1] = byte(r >> 8) //nolint:gomnd
	}

	return b
}

func zeroPathStartEnd(fpath string, start int64, last int64) error {
	fp, err := os.OpenFile(fpath, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer fp.Close()

	return zeroStartEnd(fp, start, last)
}

// zeroStartEnd - zero the start and end provided with 1MiB bytes of zeros.
func zeroStartEnd(fp io.WriteSeeker, start int64, last int64) error {
	if last <= start {
		return fmt.Errorf("last %d < start %d", last, start)
	}

	wlen := int64(disko.Mebibyte)
	bufZero := make([]byte, wlen)

	// 3 cases.
	// a.) start + wlen < last - wlen (two full writes)
	// b.) start + wlen >= last (one possibly short write)
	// c.) start + wlen >= last - wlen (overlapping zero ranges)
	type ws struct{ start, size int64 }
	var writes = []ws{{start, wlen}, {last - wlen, wlen}}
	var wnum int
	var err error

	if start+wlen >= last {
		writes = []ws{{start, last - start}}
	} else if start+wlen >= last-wlen {
		writes = []ws{{start, wlen}, {start + wlen, last - (start + wlen)}}
	}

	for _, w := range writes {
		if _, err = fp.Seek(w.start, io.SeekStart); err != nil {
			return fmt.Errorf("failed to seek to %d to write %v", w.start, w)
		}

		wnum, err = fp.Write(bufZero[:w.size])
		if err != nil {
			return fmt.Errorf("failed to write %v", w)
		}

		if int64(wnum) != w.size {
			return fmt.Errorf("wrote only %d bytes of %v", wnum, w)
		}
	}

	return nil
}

// addPartitionSet - open the disk, add partitions.
//     Caller's responsibility to udevSettle
func addPartitionSet(d disko.Disk, pSet disko.PartitionSet) error {
	fp, err := os.OpenFile(d.Path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer fp.Close()

	if err := syscall.Flock(int(fp.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("failed to lock %s: %s", d.Path, err)
	}

	gptTable, _, err := readTableSearch(fp, []uint{d.SectorSize})
	if err == ErrNoPartitionTable {
		gptTable, err = writeNewGPTTable(fp, d.SectorSize, d.Size)
		if err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	for _, p := range pSet {
		gptTable.Partitions[p.Number-1] = toGPTPartition(p, d.SectorSize)

		if err := zeroStartEnd(fp, int64(p.Start), int64(p.Last)); err != nil {
			return fmt.Errorf("failed to zero partition %d: %s", p.ID, err)
		}
	}

	_, err = writeGPTTable(fp, gptTable)

	if err != nil {
		return err
	}

	// close the file handle, releasing the lock before calling udevSettle
	// https://systemd.io/BLOCK_DEVICE_LOCKING/
	return fp.Close()
}

func deletePartitions(d disko.Disk, pNums []uint) error {
	fp, err := os.OpenFile(d.Path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer fp.Close()

	if err := syscall.Flock(int(fp.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("failed to lock %s: %s", d.Path, err)
	}

	gptTable, _, err := readTableSearch(fp, []uint{d.SectorSize})
	if err != nil {
		return err
	}

	emptyPart := toGPTPartition(
		disko.Partition{
			Start: 0,
			Last:  0,
			ID:    disko.GUID{0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0, 0x0},
			Type:  partid.Empty,
		}, d.SectorSize)

	for _, pNum := range pNums {
		gptTable.Partitions[pNum-1] = emptyPart
	}

	_, err = writeGPTTable(fp, gptTable)

	return err
}

// writeProtectiveMBR - add a ProtectiveMBR spanning the disk.
func writeProtectiveMBR(fp io.ReadWriteSeeker, sectorSize uint, diskSize uint64) error {
	buf := make([]byte, sectorSize)

	if _, err := fp.Seek(0, io.SeekStart); err != nil {
		return err
	}

	if _, err := io.ReadFull(fp, buf); err != nil {
		return err
	}

	m, err := newProtectiveMBR(buf, sectorSize, diskSize)
	if err != nil {
		return err
	}

	if _, err := fp.Seek(0, io.SeekStart); err != nil {
		return err
	}

	return m.Write(fp)
}

func writeNewGPTTable(fp io.ReadWriteSeeker, sectorSize uint, diskSize uint64) (gpt.Table, error) {
	ntArgs := gpt.NewTableArgs{
		SectorSize: uint64(sectorSize),
		DiskGuid:   gpt.Guid(disko.GenGUID())}
	gptTable := gpt.NewTable(diskSize, &ntArgs)

	if err := writeProtectiveMBR(fp, sectorSize, diskSize); err != nil {
		return gptTable, err
	}

	return writeGPTTable(fp, gptTable)
}

func writeGPTTable(fp io.ReadWriteSeeker, table gpt.Table) (gpt.Table, error) {
	if err := table.Write(fp); err != nil {
		fmt.Fprintf(os.Stderr, "Failed write to table: %s\n", err)
		return gpt.Table{}, err
	}

	if err := table.CreateOtherSideTable().Write(fp); err != nil {
		fmt.Fprintf(os.Stderr, "Failed write other side table: %s\n", err)
		return gpt.Table{}, err
	}

	if _, err := fp.Seek(int64(table.Header.HeaderCopyStartLBA), io.SeekStart); err != nil {
		return gpt.Table{}, err
	}

	if _, err := fp.Seek(
		int64(table.Header.HeaderStartLBA*table.SectorSize),
		io.SeekStart); err != nil {
		return gpt.Table{}, err
	}

	return gpt.ReadTable(io.ReadSeeker(fp), table.SectorSize)
}

// newProtectiveMBR - return a Protective MBR for the
// pull request to upstream mbr at https://github.com/rekby/mbr/pull/2
func newProtectiveMBR(buf []byte, sectorSize uint, diskSize uint64) (mbr.MBR, error) {
	if len(buf) < int(sectorSize) {
		return mbr.MBR{},
			fmt.Errorf("buffer too small. Must be sectorSize(%d)", sectorSize)
	}

	// error is ignored here but checked below
	myMBR, _ := mbr.Read(bytes.NewReader(buf))

	myMBR.FixSignature()

	pt := myMBR.GetPartition(1)
	pt.SetType(mbr.PART_GPT)
	pt.SetLBAStart(1)
	// Upstream pull request would set this to '- 1', not '- 2' as
	// is commonly written by linux partitioners although actually outside spec.
	pt.SetLBALen(uint32(diskSize/uint64(sectorSize)) - 2) // nolint: gomnd

	for pnum := 2; pnum <= 4; pnum++ {
		pt := myMBR.GetPartition(pnum)
		pt.SetType(mbr.PART_EMPTY)
		pt.SetLBAStart(0)
		pt.SetLBALen(0)
	}

	return *myMBR, myMBR.Check()
}
