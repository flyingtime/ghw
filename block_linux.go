// Use and distribution licensed under the Apache license version 2.
//
// See the COPYING file in the root project directory for full text.
//

package ghw

import (
	"bufio"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/jaypipes/ghw/pkg/context"
	"github.com/jaypipes/ghw/pkg/linuxpath"
	"github.com/jaypipes/ghw/pkg/util"
)

const (
	sectorSize = 512
)

func blockFillInfo(ctx *context.Context, info *BlockInfo) error {
	paths := linuxpath.New(ctx)
	info.Disks = disks(paths)
	var tpb uint64
	for _, d := range info.Disks {
		tpb += d.SizeBytes
	}
	info.TotalPhysicalBytes = tpb
	return nil
}

func diskPhysicalBlockSizeBytes(paths *linuxpath.Paths, disk string) uint64 {
	// We can find the sector size in Linux by looking at the
	// /sys/block/$DEVICE/queue/physical_block_size file in sysfs
	path := filepath.Join(paths.SysBlock, disk, "queue", "physical_block_size")
	contents, err := ioutil.ReadFile(path)
	if err != nil {
		return 0
	}
	size, err := strconv.ParseUint(strings.TrimSpace(string(contents)), 10, 64)
	if err != nil {
		return 0
	}
	return size
}

func diskSizeBytes(paths *linuxpath.Paths, disk string) uint64 {
	// We can find the number of 512-byte sectors by examining the contents of
	// /sys/block/$DEVICE/size and calculate the physical bytes accordingly.
	path := filepath.Join(paths.SysBlock, disk, "size")
	contents, err := ioutil.ReadFile(path)
	if err != nil {
		return 0
	}
	size, err := strconv.ParseUint(strings.TrimSpace(string(contents)), 10, 64)
	if err != nil {
		return 0
	}
	return size * sectorSize
}

func diskNUMANodeID(paths *linuxpath.Paths, disk string) int {
	link, err := os.Readlink(filepath.Join(paths.SysBlock, disk))
	if err != nil {
		return -1
	}
	for partial := link; strings.HasPrefix(partial, "../devices/"); partial = filepath.Base(partial) {
		if nodeContents, err := ioutil.ReadFile(filepath.Join(paths.SysBlock, partial, "numa_node")); err != nil {
			if nodeInt, err := strconv.Atoi(string(nodeContents)); err != nil {
				return nodeInt
			}
		}
	}
	return -1
}

func diskVendor(paths *linuxpath.Paths, disk string) string {
	// In Linux, the vendor for a disk device is found in the
	// /sys/block/$DEVICE/device/vendor file in sysfs
	path := filepath.Join(paths.SysBlock, disk, "device", "vendor")
	contents, err := ioutil.ReadFile(path)
	if err != nil {
		return UNKNOWN
	}
	return strings.TrimSpace(string(contents))
}

func udevInfo(paths *linuxpath.Paths, disk string) (map[string]string, error) {
	// Get device major:minor numbers
	devNo, err := ioutil.ReadFile(filepath.Join(paths.SysBlock, disk, "dev"))
	if err != nil {
		return nil, err
	}

	// Look up block device in udev runtime database
	udevID := "b" + strings.TrimSpace(string(devNo))
	udevBytes, err := ioutil.ReadFile(filepath.Join(paths.RunUdevData, udevID))
	if err != nil {
		return nil, err
	}

	udevInfo := make(map[string]string)
	for _, udevLine := range strings.Split(string(udevBytes), "\n") {
		if strings.HasPrefix(udevLine, "E:") {
			if s := strings.SplitN(udevLine[2:], "=", 2); len(s) == 2 {
				udevInfo[s[0]] = s[1]
			}
		}
	}
	return udevInfo, nil
}

func diskModel(paths *linuxpath.Paths, disk string) string {
	info, err := udevInfo(paths, disk)
	if err != nil {
		return UNKNOWN
	}

	if model, ok := info["ID_MODEL"]; ok {
		return model
	}
	return UNKNOWN
}

func diskSerialNumber(paths *linuxpath.Paths, disk string) string {
	info, err := udevInfo(paths, disk)
	if err != nil {
		return UNKNOWN
	}

	// There are two serial number keys, ID_SERIAL and ID_SERIAL_SHORT
	// The non-_SHORT version often duplicates vendor information collected elsewhere, so use _SHORT.
	if serial, ok := info["ID_SERIAL_SHORT"]; ok {
		return serial
	}
	return UNKNOWN
}

func diskBusPath(paths *linuxpath.Paths, disk string) string {
	info, err := udevInfo(paths, disk)
	if err != nil {
		return UNKNOWN
	}

	// There are two path keys, ID_PATH and ID_PATH_TAG.
	// The difference seems to be _TAG has funky characters converted to underscores.
	if path, ok := info["ID_PATH"]; ok {
		return path
	}
	return UNKNOWN
}

func diskWWN(paths *linuxpath.Paths, disk string) string {
	info, err := udevInfo(paths, disk)
	if err != nil {
		return UNKNOWN
	}

	// Trying ID_WWN_WITH_EXTENSION and falling back to ID_WWN is the same logic lsblk uses
	if wwn, ok := info["ID_WWN_WITH_EXTENSION"]; ok {
		return wwn
	}
	if wwn, ok := info["ID_WWN"]; ok {
		return wwn
	}
	return UNKNOWN
}

// diskPartitions takes the name of a disk (note: *not* the path of the disk,
// but just the name. In other words, "sda", not "/dev/sda" and "nvme0n1" not
// "/dev/nvme0n1") and returns a slice of pointers to Partition structs
// representing the partitions in that disk
func diskPartitions(paths *linuxpath.Paths, disk string) []*Partition {
	out := make([]*Partition, 0)
	path := filepath.Join(paths.SysBlock, disk)
	files, err := ioutil.ReadDir(path)
	if err != nil {
		util.Warn("failed to read disk partitions: %s\n", err)
		return out
	}
	for _, file := range files {
		fname := file.Name()
		if !strings.HasPrefix(fname, disk) {
			continue
		}
		size := partitionSizeBytes(paths, disk, fname)
		mp, pt, ro := partitionInfo(paths, fname)
		p := &Partition{
			Name:       fname,
			SizeBytes:  size,
			MountPoint: mp,
			Type:       pt,
			IsReadOnly: ro,
		}
		out = append(out, p)
	}
	return out
}

func diskIsRemovable(paths *linuxpath.Paths, disk string) bool {
	path := filepath.Join(paths.SysBlock, disk, "removable")
	contents, err := ioutil.ReadFile(path)
	if err != nil {
		return false
	}
	removable := strings.TrimSpace(string(contents))
	if removable == "1" {
		return true
	}
	return false
}

func disks(paths *linuxpath.Paths) []*Disk {
	// In Linux, we could use the fdisk, lshw or blockdev commands to list disk
	// information, however all of these utilities require root privileges to
	// run. We can get all of this information by examining the /sys/block
	// and /sys/class/block files
	disks := make([]*Disk, 0)
	files, err := ioutil.ReadDir(paths.SysBlock)
	if err != nil {
		return nil
	}
	for _, file := range files {
		dname := file.Name()
		if strings.HasPrefix(dname, "loop") {
			continue
		}

		driveType, storageController := diskTypes(dname)
		// TODO(jaypipes): Move this into diskTypes() once abstracting
		// diskIsRotational for ease of unit testing
		if !diskIsRotational(paths, dname) {
			driveType = DRIVE_TYPE_SSD
		}
		size := diskSizeBytes(paths, dname)
		pbs := diskPhysicalBlockSizeBytes(paths, dname)
		busPath := diskBusPath(paths, dname)
		node := diskNUMANodeID(paths, dname)
		vendor := diskVendor(paths, dname)
		model := diskModel(paths, dname)
		serialNo := diskSerialNumber(paths, dname)
		wwn := diskWWN(paths, dname)
		removable := diskIsRemovable(paths, dname)

		d := &Disk{
			Name:                   dname,
			SizeBytes:              size,
			PhysicalBlockSizeBytes: pbs,
			DriveType:              driveType,
			IsRemovable:            removable,
			StorageController:      storageController,
			BusPath:                busPath,
			NUMANodeID:             node,
			Vendor:                 vendor,
			Model:                  model,
			SerialNumber:           serialNo,
			WWN:                    wwn,
		}

		parts := diskPartitions(paths, dname)
		// Map this Disk object into the Partition...
		for _, part := range parts {
			part.Disk = d
		}
		d.Partitions = parts

		disks = append(disks, d)
	}

	return disks
}

// diskTypes returns the drive type, storage controller and bus type of a disk
func diskTypes(dname string) (
	DriveType,
	StorageController,
) {
	// The conditionals below which set the controller and drive type are
	// based on information listed here:
	// https://en.wikipedia.org/wiki/Device_file
	driveType := DRIVE_TYPE_UNKNOWN
	storageController := STORAGE_CONTROLLER_UNKNOWN
	if strings.HasPrefix(dname, "fd") {
		driveType = DRIVE_TYPE_FDD
	} else if strings.HasPrefix(dname, "sd") {
		driveType = DRIVE_TYPE_HDD
		storageController = STORAGE_CONTROLLER_SCSI
	} else if strings.HasPrefix(dname, "hd") {
		driveType = DRIVE_TYPE_HDD
		storageController = STORAGE_CONTROLLER_IDE
	} else if strings.HasPrefix(dname, "vd") {
		driveType = DRIVE_TYPE_HDD
		storageController = STORAGE_CONTROLLER_VIRTIO
	} else if strings.HasPrefix(dname, "nvme") {
		driveType = DRIVE_TYPE_SSD
		storageController = STORAGE_CONTROLLER_NVME
	} else if strings.HasPrefix(dname, "sr") {
		driveType = DRIVE_TYPE_ODD
		storageController = STORAGE_CONTROLLER_SCSI
	} else if strings.HasPrefix(dname, "xvd") {
		driveType = DRIVE_TYPE_HDD
		storageController = STORAGE_CONTROLLER_SCSI
	} else if strings.HasPrefix(dname, "mmc") {
		driveType = DRIVE_TYPE_SSD
		storageController = STORAGE_CONTROLLER_MMC
	}

	return driveType, storageController
}

func diskIsRotational(paths *linuxpath.Paths, devName string) bool {
	path := filepath.Join(paths.SysBlock, devName, "queue", "rotational")
	contents := util.SafeIntFromFile(path)
	return contents == 1
}

// partitionSizeBytes returns the size in bytes of the partition given a disk
// name and a partition name. Note: disk name and partition name do *not*
// contain any leading "/dev" parts. In other words, they are *names*, not
// paths.
func partitionSizeBytes(paths *linuxpath.Paths, disk string, part string) uint64 {
	path := filepath.Join(paths.SysBlock, disk, part, "size")
	contents, err := ioutil.ReadFile(path)
	if err != nil {
		return 0
	}
	size, err := strconv.ParseUint(strings.TrimSpace(string(contents)), 10, 64)
	if err != nil {
		return 0
	}
	return size * sectorSize
}

// Given a full or short partition name, returns the mount point, the type of
// the partition and whether it's readonly
func partitionInfo(paths *linuxpath.Paths, part string) (string, string, bool) {
	// Allow calling PartitionInfo with either the full partition name
	// "/dev/sda1" or just "sda1"
	if !strings.HasPrefix(part, "/dev") {
		part = "/dev/" + part
	}

	// /etc/mtab entries for mounted partitions look like this:
	// /dev/sda6 / ext4 rw,relatime,errors=remount-ro,data=ordered 0 0
	var r io.ReadCloser
	r, err := os.Open(paths.EtcMtab)
	if err != nil {
		return "", "", true
	}
	defer util.SafeClose(r)

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := scanner.Text()
		entry := parseMtabEntry(line)
		if entry == nil || entry.Partition != part {
			continue
		}
		ro := true
		for _, opt := range entry.Options {
			if opt == "rw" {
				ro = false
				break
			}
		}

		return entry.Mountpoint, entry.FilesystemType, ro
	}
	return "", "", true
}

type mtabEntry struct {
	Partition      string
	Mountpoint     string
	FilesystemType string
	Options        []string
}

func parseMtabEntry(line string) *mtabEntry {
	// /etc/mtab entries for mounted partitions look like this:
	// /dev/sda6 / ext4 rw,relatime,errors=remount-ro,data=ordered 0 0
	if line[0] != '/' {
		return nil
	}
	fields := strings.Fields(line)

	if len(fields) < 4 {
		return nil
	}

	// We do some special parsing of the mountpoint, which may contain space,
	// tab and newline characters, encoded into the mtab entry line using their
	// octal-to-string representations. From the GNU mtab man pages:
	//
	//   "Therefore these characters are encoded in the files and the getmntent
	//   function takes care of the decoding while reading the entries back in.
	//   '\040' is used to encode a space character, '\011' to encode a tab
	//   character, '\012' to encode a newline character, and '\\' to encode a
	//   backslash."
	mp := fields[1]
	r := strings.NewReplacer(
		"\\011", "\t", "\\012", "\n", "\\040", " ", "\\\\", "\\",
	)
	mp = r.Replace(mp)

	res := &mtabEntry{
		Partition:      fields[0],
		Mountpoint:     mp,
		FilesystemType: fields[2],
	}
	opts := strings.Split(fields[3], ",")
	res.Options = opts
	return res
}

func partitionMountPoint(paths *linuxpath.Paths, part string) string {
	mp, _, _ := partitionInfo(paths, part)
	return mp
}

func partitionType(paths *linuxpath.Paths, part string) string {
	_, pt, _ := partitionInfo(paths, part)
	return pt
}

func partitionIsReadOnly(paths *linuxpath.Paths, part string) bool {
	_, _, ro := partitionInfo(paths, part)
	return ro
}
