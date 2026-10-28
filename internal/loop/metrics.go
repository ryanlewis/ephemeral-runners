package loop

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
)

// writeMetrics writes the Prometheus textfile named by metrics_file, if
// any. It writes a temporary file and renames it, so a reader never sees
// half a file.
func (l *Loop) writeMetrics() {
	path := l.cfg.MetricsFile
	if path == "" {
		return
	}
	if err := writeFileAtomic(path, l.metrics()); err != nil {
		l.log.Warn("write metrics", "file", path, "err", err)
	}
}

func (l *Loop) metrics() []byte {
	count := map[[2]string]int{}
	spares := 0
	for _, m := range l.machines {
		count[[2]string{m.state.String(), m.size}]++
		if m.state == spare {
			spares++
		}
	}
	var b bytes.Buffer
	b.WriteString("# HELP ephemeral_runners_vms Runner VMs by state and size.\n")
	b.WriteString("# TYPE ephemeral_runners_vms gauge\n")
	for _, st := range stateNames {
		for _, size := range l.cfg.SizeNames() {
			fmt.Fprintf(&b, "ephemeral_runners_vms{state=%q,size=%q} %d\n", st, size, count[[2]string{st, size}])
		}
	}
	b.WriteString("# HELP ephemeral_runners_spares Warm spare VMs waiting for a job.\n")
	b.WriteString("# TYPE ephemeral_runners_spares gauge\n")
	fmt.Fprintf(&b, "ephemeral_runners_spares %d\n", spares)
	b.WriteString("# HELP ephemeral_runners_jobs_started_total Job configs handed to VMs.\n")
	b.WriteString("# TYPE ephemeral_runners_jobs_started_total counter\n")
	fmt.Fprintf(&b, "ephemeral_runners_jobs_started_total %d\n", l.jobsStarted)
	b.WriteString("# HELP ephemeral_runners_launch_failures_total Failed attempts to start a VM or hand it a job.\n")
	b.WriteString("# TYPE ephemeral_runners_launch_failures_total counter\n")
	fmt.Fprintf(&b, "ephemeral_runners_launch_failures_total %d\n", l.launchFailures)
	b.WriteString("# HELP ephemeral_runners_last_poll_success_seconds Unix time of the last successful poll of GitHub.\n")
	b.WriteString("# TYPE ephemeral_runners_last_poll_success_seconds gauge\n")
	last := int64(0)
	if !l.lastPoll.IsZero() {
		last = l.lastPoll.Unix()
	}
	fmt.Fprintf(&b, "ephemeral_runners_last_poll_success_seconds %d\n", last)
	return b.Bytes()
}

func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(0o644); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
