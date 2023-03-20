// Copyright 2022 Chaos Mesh Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"github.com/containerd/cgroups"
	v1 "github.com/containerd/cgroups/stats/v1"
	"github.com/dustin/go-humanize"
	psutil "github.com/shirou/gopsutil/mem"
)

var (
	requireCgroupLimit bool
	pidNum             int
	memSize            string
	growthTime         string
	workers            int
	client             bool
)

func init() {
	flag.StringVar(&memSize, "size", "0KB", "size of memory you want to allocate")
	flag.IntVar(&pidNum, "pid", 0, "container pid numer")
	flag.StringVar(&growthTime, "time", "0s", "time to reach the size of memory you allocated")
	flag.IntVar(&workers, "workers", 1, "number of workers allocating memory")
	flag.BoolVar(&client, "client", false, "the process runs as a client")
	flag.BoolVar(&requireCgroupLimit, "required-limit", true, "required container has "+
		"resource limit")
	flag.Parse()
}

func linearGrow(data []byte, length uint64, timeLine time.Duration) {
	startTime := time.Now()
	endTime := startTime.Add(timeLine)

	var allocated uint64 = 0
	pageSize := uint64(syscall.Getpagesize())
	interval := time.Millisecond * 10

	for {
		now := time.Now()
		if now.After(endTime) {
			now = endTime
		}
		expected := length * uint64(now.Sub(startTime).Milliseconds()) /
			uint64(endTime.Sub(startTime).Milliseconds()) / pageSize

		for i := allocated; uint64(i) < expected; i++ {
			data[uint64(i)*pageSize] = 0
		}

		allocated = expected
		if now.Equal(endTime) {
			break
		} else {
			time.Sleep(interval)
		}
	}

}

func run(length uint64, timeLine time.Duration) {
	data, err := syscall.Mmap(-1, 0, int(length), syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS)
	if err != nil {
		// TODO
		fmt.Println(err)
		os.Exit(1)
	}

	if timeLine > time.Nanosecond {
		linearGrow(data, length, timeLine)
	} else {
		sysPageSize := os.Getpagesize()
		for i := 0; uint64(i) < length; i += sysPageSize {
			data[i] = 0
		}
	}

	for {
		time.Sleep(time.Second * 2)
	}
}

func main() {
	if err := checkOptions(); err != nil {
		exitWithError(err)
	}

	if !client {
		fmt.Printf("size: %s, workers: %d, pid: %d, client: %t, requiredLimit: %t\n",
			memSize, workers, pidNum, client, requireCgroupLimit)
		workQueue := make(chan struct{}, workers)
		for {
			workQueue <- struct{}{}
			go func() {
				args := []string{
					"--size=" + memSize,
					"--workers=" + fmt.Sprintf("%d", workers),
					"--time=" + growthTime,
					"--client=" + "true",
					"--pid=" + fmt.Sprintf("%d", pidNum),
				}

				if !requireCgroupLimit {
					args = append(args, "--required-limit="+"false")
				}

				cmd := exec.Command("memStress", args...)
				cmd.SysProcAttr = &syscall.SysProcAttr{
					Pdeathsig: syscall.SIGTERM,
				}
				outputs, err := cmd.CombinedOutput()
				if err != nil {
					exitWithError(fmt.Errorf("output: %s, err: %s", string(outputs), err.Error()))
				}
				<-workQueue
			}()
			time.Sleep(time.Second)
		}
	} else {
		percentage, expectBytes := parseSize(memSize)
		filledSize, err := calculateMemSize(percentage, expectBytes)
		if err != nil {
			exitWithError(err)
		}

		timeLine, err := time.ParseDuration(growthTime)
		if err != nil {
			exitWithError(err)
		}
		run(filledSize/uint64(workers), timeLine)
	}
}

func checkOptions() error {
	if memSize == "" {
		return fmt.Errorf("options size is required")
	}

	if workers == 0 {
		return fmt.Errorf("workers required lager than one")
	}

	if _, err := time.ParseDuration(growthTime); err != nil {
		return fmt.Errorf("bad time format")
	}

	if memSize[len(memSize)-1] == '%' {
		if pidNum == 0 {
			return fmt.Errorf("pid number is required when size is the percent value")
		}
		percentage, err := strconv.ParseFloat(memSize[0:len(memSize)-1], 64)
		if err != nil {
			return fmt.Errorf("bad percent format")
		}
		if percentage > 100 || percentage == 0 {
			return fmt.Errorf("percent value required range (0, 100]")
		}
	}
	if memSize[len(memSize)-1] != '%' {
		length, err := humanize.ParseBytes(memSize)
		if err != nil {
			return fmt.Errorf("bad size format")
		}
		if length == 0 {
			return fmt.Errorf("size is zero value")
		}
	}
	return nil
}

func exitWithError(err error) {
	_, _ = fmt.Fprintf(os.Stderr, err.Error())
	os.Exit(1)
}

// parseSize parse size
// Two size formats are supported, which are 100MB or 30%
// @Author MarsDong 2023-03-20 17:08:11
func parseSize(memSize string) (float64, uint64) {
	if memSize[len(memSize)-1] != '%' {
		length, _ := humanize.ParseBytes(memSize)
		return 0, length
	}
	percentage, _ := strconv.ParseFloat(memSize[0:len(memSize)-1], 64)
	return percentage, 0
}

// calculateMemSize get actual filled size
// If the user specifies the size of memory to fill, it fills directly, and if a percentage is specified,
// the remaining size to fill needs to be calculated based on the current memory usage.
// @Author MarsDong 2023-03-20 16:11:00
func calculateMemSize(percent float64, expectUsed uint64) (uint64, error) {
	if expectUsed > 0 {
		return expectUsed, nil
	}

	nodeMemory, err := getMemFromNode()
	if err != nil {
		return 0, err
	}
	cgroupMemory, err := getMemFromCgroup(pidNum)
	if err != nil {
		return 0, err
	}

	total := nodeMemory.Total
	used := nodeMemory.Used

	// an error is returned if the container does not specify a resource limit
	if cgroupMemory.Usage.Limit < nodeMemory.Total {
		total = cgroupMemory.Usage.Limit
		used = cgroupMemory.Usage.Usage
	} else if requireCgroupLimit {
		return 0, fmt.Errorf("require resource limit")
	}

	currentUsedPercent, _ := strconv.ParseFloat(fmt.Sprintf("%.2f", float64(used)/float64(total)), 64)
	if currentUsedPercent > percent {
		return 0, fmt.Errorf("current used percent %.2f larger than expect %.2f", currentUsedPercent, percent)
	}

	actualFilled := uint64(((percent - currentUsedPercent) / 100.0) * float64(total))
	fmt.Fprintf(os.Stdout, "total: %dKB, used: %dKB, actualFilled: %dKB\n", total, used, actualFilled)
	return actualFilled, nil
}

// getMemFromNode get total memory from k8s node
// @Author MarsDong 2023-03-20 16:10:35
func getMemFromNode() (*psutil.VirtualMemoryStat, error) {
	memInfo, err := psutil.VirtualMemory()
	if err != nil {
		return nil, err
	}
	return memInfo, nil
}

// getMemFromCgroup get memory limit from cgroup configuration
// @Author MarsDong 2023-03-20 16:10:14
func getMemFromCgroup(pidNum int) (*v1.MemoryStat, error) {
	pathMap, err := cgroups.ParseCgroupFile(fmt.Sprintf("/proc/%d/cgroup", pidNum))
	if err != nil {
		return nil, err
	}

	memRoot, exists := pathMap["memory"]
	if !exists {
		return nil, fmt.Errorf("not found root path for memory")
	}

	cgroupIns, err := cgroups.Load(cgroups.V1, cgroups.StaticPath(memRoot))
	if err != nil {
		return nil, err
	}

	stats, err := cgroupIns.Stat(cgroups.IgnoreNotExist)
	if err != nil {
		return nil, err
	}

	return stats.Memory, nil
}
