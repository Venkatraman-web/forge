package main

// Run metadata that explains differences between runs on the same code (host boot
// time, load, CPU governor, container engine and VM size, exactly which sources and
// binaries were measured). Anything that cannot be read is recorded as
// "unavailable (reason)" and never guessed.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

func unavail(why string) string { return "unavailable (" + why + ")" }

// BinaryInfo identifies one executable that was built and measured.
type BinaryInfo struct {
	Name      string `json:"name"`
	SHA256    string `json:"sha256"`
	BuildInfo string `json:"build_info"` // go version and build flags from `go version -m`
}

// HostMeta is the host/engine state at the start of the run.
type HostMeta struct {
	BootTime        string `json:"host_boot_time"`
	UptimeAtStart   string `json:"host_uptime_at_start"`
	LoadAvgAtStart  string `json:"host_loadavg_at_start"`
	CPUGovernor     string `json:"cpu_scaling_governor"`
	DockerEngine    string `json:"docker_engine"`
	DockerVMCPUs    string `json:"docker_vm_cpus"`
	DockerVMMemory  string `json:"docker_vm_memory_bytes"`
	DockerVMKernel  string `json:"docker_vm_kernel"`
	DockerVMProcess string `json:"docker_vm_process_uptime"`
	SourceTreeHash  string `json:"source_tree_sha256"`
	SourceTreeRule  string `json:"source_tree_rule"`
	SourceFiles     int    `json:"source_files_hashed"`
}

func bootAndUptime() (boot, up string) {
	boot, up = unavail("no btime in /proc/stat"), unavail("cannot read /proc/uptime")
	if b, err := os.ReadFile("/proc/stat"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(l, "btime ") {
				if n, err := strconv.ParseInt(strings.Fields(l)[1], 10, 64); err == nil {
					boot = time.Unix(n, 0).Format(time.RFC3339)
				}
			}
		}
	}
	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			if s, err := strconv.ParseFloat(f[0], 64); err == nil {
				up = (time.Duration(s) * time.Second).Round(time.Second).String()
			}
		}
	}
	return boot, up
}

func cpuGovernor() string {
	b, err := os.ReadFile("/sys/devices/system/cpu/cpu0/cpufreq/scaling_governor")
	if err != nil {
		return unavail("cpufreq not exposed by this kernel/VM")
	}
	return strings.TrimSpace(string(b))
}

// dockerEngineMeta reads what the container engine reports about itself. With Docker
// Desktop the engine runs in a VM, whose CPU count and memory bound the containers.
func dockerEngineMeta(ctx context.Context) (engine, cpus, mem, kernel, proc string) {
	engine, cpus, mem, kernel = unavail("docker info failed"), unavail("docker info failed"), unavail("docker info failed"), unavail("docker info failed")
	out, err := run(ctx, "", nil, "docker", "info", "--format", "{{.OperatingSystem}}|{{.ServerVersion}}|{{.NCPU}}|{{.MemTotal}}|{{.KernelVersion}}")
	if err == nil {
		if f := strings.Split(strings.TrimSpace(out), "|"); len(f) == 5 {
			engine, cpus, mem, kernel = f[0]+" (server "+f[1]+")", f[2], f[3], f[4]
		}
	}
	proc = unavail("no qemu VM process found: the engine may run natively, or the process name differs")
	if out, err := run(ctx, "", nil, "ps", "-eo", "etimes=,comm="); err == nil {
		for _, l := range strings.Split(out, "\n") {
			if f := strings.Fields(l); len(f) == 2 && strings.Contains(f[1], "qemu-system") {
				if n, err := strconv.Atoi(f[0]); err == nil {
					proc = fmt.Sprintf("%s running for %s", f[1], (time.Duration(n) * time.Second).String())
				}
				break
			}
		}
	}
	return engine, cpus, mem, kernel, proc
}

const sourceTreeRule = "sha256 over (relative path, file sha256) of every .go/.sql/.hcl/.mod/.sum/.json/.yaml/.yml/.sh file under the repository root, sorted by path, excluding .git and forgecart/test_files/{results,audits}"

// sourceTreeHash fingerprints the sources that were built (the repository has no VCS revision to record).
func sourceTreeHash(repo string) (string, int) {
	type ent struct{ rel, sum string }
	var ents []ent
	skip := map[string]bool{".git": true, filepath.Join("forgecart", "test_files", "results"): true, filepath.Join("forgecart", "test_files", "audits"): true}
	ok := map[string]bool{".go": true, ".sql": true, ".hcl": true, ".mod": true, ".sum": true, ".json": true, ".yaml": true, ".yml": true, ".sh": true}
	err := filepath.WalkDir(repo, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(repo, p)
		if d.IsDir() {
			if skip[rel] {
				return filepath.SkipDir
			}
			return nil
		}
		if !ok[filepath.Ext(p)] {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		h := sha256.Sum256(b)
		ents = append(ents, ent{rel, hex.EncodeToString(h[:])})
		return nil
	})
	if err != nil {
		return unavail("walking the source tree failed: " + err.Error()), 0
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].rel < ents[j].rel })
	h := sha256.New()
	for _, e := range ents {
		fmt.Fprintf(h, "%s\x00%s\n", e.rel, e.sum)
	}
	return hex.EncodeToString(h.Sum(nil)), len(ents)
}

// binaryInfo hashes a built executable and records its Go build settings.
func binaryInfo(ctx context.Context, path string) BinaryInfo {
	bi := BinaryInfo{Name: filepath.Base(path)}
	b, err := os.ReadFile(path)
	if err != nil {
		bi.SHA256, bi.BuildInfo = unavail("cannot read binary"), unavail("cannot read binary")
		return bi
	}
	h := sha256.Sum256(b)
	bi.SHA256 = hex.EncodeToString(h[:])
	out, err := run(ctx, "", nil, "go", "version", "-m", path)
	if err != nil {
		bi.BuildInfo = unavail("go version -m failed")
		return bi
	}
	var parts []string
	for i, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if i == 0 {
			parts = append(parts, strings.TrimPrefix(l, path+": "))
		} else if strings.HasPrefix(l, "build") {
			parts = append(parts, strings.ReplaceAll(strings.TrimPrefix(l, "build"), "\t", " "))
		}
	}
	bi.BuildInfo = strings.Join(parts, ";")
	return bi
}

func collectHostMeta(ctx context.Context, repo string) HostMeta {
	m := HostMeta{LoadAvgAtStart: readLoadAvg(), CPUGovernor: cpuGovernor(), SourceTreeRule: sourceTreeRule}
	m.BootTime, m.UptimeAtStart = bootAndUptime()
	m.DockerEngine, m.DockerVMCPUs, m.DockerVMMemory, m.DockerVMKernel, m.DockerVMProcess = dockerEngineMeta(ctx)
	m.SourceTreeHash, m.SourceFiles = sourceTreeHash(repo)
	return m
}
