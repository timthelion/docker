// +build linux

package systemd

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	systemd1 "github.com/coreos/go-systemd/dbus"
	"github.com/dotcloud/docker/pkg/libcontainer/cgroups"
	"github.com/dotcloud/docker/pkg/libcontainer/devices"
	"github.com/dotcloud/docker/pkg/systemd"
	"github.com/godbus/dbus"
)

type systemdCgroup struct {
	cleanupDirs []string
}

var (
	connLock              sync.Mutex
	theConn               *systemd1.Conn
	hasStartTransientUnit bool
)

func UseSystemd() bool {
	if !systemd.SdBooted() {
		return false
	}

	connLock.Lock()
	defer connLock.Unlock()

	if theConn == nil {
		var err error
		theConn, err = systemd1.New()
		if err != nil {
			return false
		}

		// Assume we have StartTransientUnit
		hasStartTransientUnit = true

		// But if we get UnknownMethod error we don't
		if _, err := theConn.StartTransientUnit("test.scope", "invalid"); err != nil {
			if dbusError, ok := err.(dbus.Error); ok {
				if dbusError.Name == "org.freedesktop.DBus.Error.UnknownMethod" {
					hasStartTransientUnit = false
				}
			}
		}
	}
	return hasStartTransientUnit
}

func getIfaceForUnit(unitName string) string {
	if strings.HasSuffix(unitName, ".scope") {
		return "Scope"
	}
	if strings.HasSuffix(unitName, ".service") {
		return "Service"
	}
	return "Unit"
}

type cgroupArg struct {
	File  string
	Value string
}

func Apply(c *cgroups.Cgroup, pid int) (cgroups.ActiveCgroup, error) {
	var (
		unitName   = getUnitName(c)
		slice      = "system.slice"
		properties []systemd1.Property
		cpuArgs    []cgroupArg
		cpusetArgs []cgroupArg
		memoryArgs []cgroupArg
		res        systemdCgroup
	)

	// First set up things not supported by systemd

	// -1 disables memorySwap
	if c.MemorySwap >= 0 && (c.Memory != 0 || c.MemorySwap > 0) {
		memorySwap := c.MemorySwap

		if memorySwap == 0 {
			// By default, MemorySwap is set to twice the size of RAM.
			memorySwap = c.Memory * 2
		}

		memoryArgs = append(memoryArgs, cgroupArg{"memory.memsw.limit_in_bytes", strconv.FormatInt(memorySwap, 10)})
	}

	if c.CpusetCpus != "" {
		cpusetArgs = append(cpusetArgs, cgroupArg{"cpuset.cpus", c.CpusetCpus})
	}

	if c.Slice != "" {
		slice = c.Slice
	}

	properties = append(properties,
		systemd1.Property{"Slice", dbus.MakeVariant(slice)},
		systemd1.Property{"Description", dbus.MakeVariant("docker container " + c.Name)},
		systemd1.Property{"PIDs", dbus.MakeVariant([]uint32{uint32(pid)})},
	)

	if !c.UnlimitedDeviceAccess {
		properties = append(properties,
			systemd1.Property{"DevicePolicy", dbus.MakeVariant("strict")})
	}

	// Always enable accounting, this gets us the same behaviour as the fs implementation,
	// plus the kernel has some problems with joining the memory cgroup at a later time.
	properties = append(properties,
		systemd1.Property{"MemoryAccounting", dbus.MakeVariant(true)},
		systemd1.Property{"CPUAccounting", dbus.MakeVariant(true)},
		systemd1.Property{"BlockIOAccounting", dbus.MakeVariant(true)})

	if c.Memory != 0 {
		properties = append(properties,
			systemd1.Property{"MemoryLimit", dbus.MakeVariant(uint64(c.Memory))})
	}
	// TODO: MemoryReservation and MemorySwap not available in systemd

	if c.CpuShares != 0 {
		properties = append(properties,
			systemd1.Property{"CPUShares", dbus.MakeVariant(uint64(c.CpuShares))})
	}

	if _, err := theConn.StartTransientUnit(unitName, "replace", properties...); err != nil {
		return nil, err
	}

	// To work around the lack of /dev/pts/* support above we need to manually add these
	// so, ask systemd for the cgroup used
	props, err := theConn.GetUnitTypeProperties(unitName, getIfaceForUnit(unitName))
	if err != nil {
		return nil, err
	}

	cgroup := props["ControlGroup"].(string)

	if !c.UnlimitedDeviceAccess {
		mountpoint, err := cgroups.FindCgroupMountpoint("devices")
		if err != nil {
			return nil, err
		}

		dir := filepath.Join(mountpoint, cgroup)
		// We use the same method of allowing devices as in the fs backend.  This needs to be changed to use DBUS as soon as possible.  However, that change has to wait untill http://cgit.freedesktop.org/systemd/systemd/commit/?id=90060676c442604780634c0a993e3f9c3733f8e6 has been applied in most commonly used systemd versions.
		for _, dev := range c.AllowedDevices {
			deviceAllowString := fmt.Sprintf("%c %s:%s %s", dev.Type, devices.GetDeviceNumberString(dev.MajorNumber), devices.GetDeviceNumberString(dev.MinorNumber), dev.CgroupPermissions)
			if err := writeFile(dir, "devices.allow", deviceAllowString); err != nil {
				return nil, err
			}
		}
	}

	if len(cpuArgs) != 0 {
		mountpoint, err := cgroups.FindCgroupMountpoint("cpu")
		if err != nil {
			return nil, err
		}

		path := filepath.Join(mountpoint, cgroup)

		for _, arg := range cpuArgs {
			if err := ioutil.WriteFile(filepath.Join(path, arg.File), []byte(arg.Value), 0700); err != nil {
				return nil, err
			}
		}
	}

	if len(memoryArgs) != 0 {
		mountpoint, err := cgroups.FindCgroupMountpoint("memory")
		if err != nil {
			return nil, err
		}

		path := filepath.Join(mountpoint, cgroup)

		for _, arg := range memoryArgs {
			if err := ioutil.WriteFile(filepath.Join(path, arg.File), []byte(arg.Value), 0700); err != nil {
				return nil, err
			}
		}
	}

	if len(cpusetArgs) != 0 {
		// systemd does not atm set up the cpuset controller, so we must manually
		// join it. Additionally that is a very finicky controller where each
		// level must have a full setup as the default for a new directory is "no cpus",
		// so we avoid using any hierarchies here, creating a toplevel directory.
		mountpoint, err := cgroups.FindCgroupMountpoint("cpuset")
		if err != nil {
			return nil, err
		}
		initPath, err := cgroups.GetInitCgroupDir("cpuset")
		if err != nil {
			return nil, err
		}

		rootPath := filepath.Join(mountpoint, initPath)

		path := filepath.Join(mountpoint, initPath, c.Parent+"-"+c.Name)

		res.cleanupDirs = append(res.cleanupDirs, path)

		if err := os.MkdirAll(path, 0755); err != nil && !os.IsExist(err) {
			return nil, err
		}

		foundCpus := false
		foundMems := false

		for _, arg := range cpusetArgs {
			if arg.File == "cpuset.cpus" {
				foundCpus = true
			}
			if arg.File == "cpuset.mems" {
				foundMems = true
			}
			if err := ioutil.WriteFile(filepath.Join(path, arg.File), []byte(arg.Value), 0700); err != nil {
				return nil, err
			}
		}

		// These are required, if not specified inherit from parent
		if !foundCpus {
			s, err := ioutil.ReadFile(filepath.Join(rootPath, "cpuset.cpus"))
			if err != nil {
				return nil, err
			}

			if err := ioutil.WriteFile(filepath.Join(path, "cpuset.cpus"), s, 0700); err != nil {
				return nil, err
			}
		}

		// These are required, if not specified inherit from parent
		if !foundMems {
			s, err := ioutil.ReadFile(filepath.Join(rootPath, "cpuset.mems"))
			if err != nil {
				return nil, err
			}

			if err := ioutil.WriteFile(filepath.Join(path, "cpuset.mems"), s, 0700); err != nil {
				return nil, err
			}
		}

		if err := ioutil.WriteFile(filepath.Join(path, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0700); err != nil {
			return nil, err
		}
	}

	return &res, nil
}

func writeFile(dir, file, data string) error {
	return ioutil.WriteFile(filepath.Join(dir, file), []byte(data), 0700)
}

func (c *systemdCgroup) Cleanup() error {
	// systemd cleans up, we don't need to do much

	for _, path := range c.cleanupDirs {
		os.RemoveAll(path)
	}

	return nil
}

func GetPids(c *cgroups.Cgroup) ([]int, error) {
	unitName := getUnitName(c)

	mountpoint, err := cgroups.FindCgroupMountpoint("cpu")
	if err != nil {
		return nil, err
	}

	props, err := theConn.GetUnitTypeProperties(unitName, getIfaceForUnit(unitName))
	if err != nil {
		return nil, err
	}
	cgroup := props["ControlGroup"].(string)

	return cgroups.ReadProcsFile(filepath.Join(mountpoint, cgroup))
}

func getUnitName(c *cgroups.Cgroup) string {
	return fmt.Sprintf("%s-%s.scope", c.Parent, c.Name)
}
