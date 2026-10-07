// Package maintenance builds the node maintenance status view (holds plus the
// work still in flight) shared by the admin API and the local CLI, and runs the
// in-agent watcher that publishes quiesce samples while the node is paused and
// warns about holds left in place too long.
package maintenance

import (
	"context"
	"fmt"
	"sort"
	"time"

	"cs-agent/log"
	"cs-agent/store"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/hashicorp/go-hclog"
	"github.com/spf13/viper"
)

// Quiesce values.
const (
	QuiesceQuiesced = "quiesced" // nothing in flight
	QuiesceBusy     = "busy"     // something still running
	QuiesceUnknown  = "unknown"  // docker unreachable; backup containers not visible
)

// In-flight kinds that are not task names.
const (
	KindMaintPrefix   = "maint."         // + maintenance job name, e.g. "maint.prune"
	KindBorgContainer = "borg.container" // a running backup container
)

// dockerListTimeout bounds the docker query so a hung daemon reports "unknown"
// instead of stalling an admin request or the watcher.
const dockerListTimeout = 5 * time.Second

func maintenanceLog() hclog.Logger {
	return log.New().Named("maintenance")
}

// InFlight is one unit of work still running on the node: a running task (Kind
// is the task name), a node maintenance job (Kind "maint.<name>") or a running
// backup container (Kind "borg.container"). A backup container that belongs to a
// running task appears under both; that is intended, the container is what holds
// the repository lock.
type InFlight struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Volume    string `json:"volume,omitempty"`
	StartedAt int64  `json:"started_at"`
}

// Status is the node's maintenance status: the stored state plus what is still
// running. Running is never null. Quiesce is "unknown" when the backup
// containers could not be listed, else "quiesced" iff Running is empty, else
// "busy".
type Status struct {
	Paused         bool                     `json:"paused"`
	Controller     *store.MaintenanceHold   `json:"controller"`
	Local          *store.MaintenanceHold   `json:"local"`
	ControllerGen  int64                    `json:"controller_gen"`
	PausedSince    int64                    `json:"paused_since,omitempty"`
	Running        []InFlight               `json:"running"`
	Pending        int                      `json:"pending"`
	SkippedBackups int64                    `json:"skipped_backups"`
	Seq            int64                    `json:"seq"`
	Quiesce        string                   `json:"quiesce"`
	Sample         *store.MaintenanceSample `json:"sample"`
	InstanceID     string                   `json:"instance_id"`
}

// ContainerLister lists the backup containers running on the node.
type ContainerLister interface {
	RunningBackupContainers(ctx context.Context) ([]InFlight, error)
}

// StatusStore is the subset of *store.Store that BuildStatus reads.
type StatusStore interface {
	GetMaintenance(ctx context.Context) (store.MaintenanceState, error)
	ListRunningTasks(ctx context.Context) ([]store.Task, error)
	ListMaintJobs(ctx context.Context) (map[string]int64, error)
	CountPendingTasks(ctx context.Context) (int, error)
}

// DockerLister returns a ContainerLister backed by the local docker daemon.
func DockerLister() ContainerLister { return dockerLister{} }

type dockerLister struct{}

// RunningBackupContainers lists running containers labeled
// com.computestacks.role=backup. ID is the container ID, Volume the volume the
// container works on (com.computestacks.for) and StartedAt its creation time.
func (dockerLister) RunningBackupContainers(ctx context.Context) ([]InFlight, error) {
	cli, err := client.NewClientWithOpts(client.WithVersion(viper.GetString("docker.version")))
	if err != nil {
		return nil, fmt.Errorf("maintenance: docker client: %w", err)
	}
	defer cli.Close()

	qctx, cancel := context.WithTimeout(ctx, dockerListTimeout)
	defer cancel()
	containers, err := cli.ContainerList(qctx, container.ListOptions{
		Filters: filters.NewArgs(
			filters.Arg("label", "com.computestacks.role=backup"),
			filters.Arg("status", "running"),
		),
	})
	if err != nil {
		return nil, fmt.Errorf("maintenance: list backup containers: %w", err)
	}
	var out []InFlight
	for _, c := range containers {
		if c.State != "running" {
			continue
		}
		out = append(out, InFlight{
			ID:        c.ID,
			Kind:      KindBorgContainer,
			Volume:    c.Labels["com.computestacks.for"],
			StartedAt: c.Created,
		})
	}
	return out, nil
}

// BuildStatus reads the stored maintenance state and assembles the full status.
func BuildStatus(ctx context.Context, st StatusStore, cl ContainerLister) (Status, error) {
	m, err := st.GetMaintenance(ctx)
	if err != nil {
		return Status{}, err
	}
	return StatusFromState(ctx, st, cl, m)
}

// StatusFromState assembles the status around an already-loaded state (e.g. the
// state a hold write returned), adding the work in flight and the pending count.
// A lister error is not an error here: it makes Quiesce "unknown".
func StatusFromState(ctx context.Context, st StatusStore, cl ContainerLister, m store.MaintenanceState) (Status, error) {
	s := Status{
		Paused:         m.Paused(),
		Controller:     m.Controller,
		Local:          m.Local,
		ControllerGen:  m.ControllerGen,
		PausedSince:    m.PausedSince,
		Running:        []InFlight{},
		SkippedBackups: m.SkippedBackups,
		Seq:            m.Seq,
		Sample:         m.Sample,
		InstanceID:     m.InstanceID,
	}

	tasks, err := st.ListRunningTasks(ctx)
	if err != nil {
		return Status{}, err
	}
	for _, t := range tasks {
		s.Running = append(s.Running, InFlight{ID: t.ID, Kind: t.Name, Volume: t.Volume, StartedAt: t.UpdatedAt})
	}

	jobs, err := st.ListMaintJobs(ctx)
	if err != nil {
		return Status{}, err
	}
	for name, started := range jobs {
		s.Running = append(s.Running, InFlight{ID: KindMaintPrefix + name, Kind: KindMaintPrefix + name, StartedAt: started})
	}

	if s.Pending, err = st.CountPendingTasks(ctx); err != nil {
		return Status{}, err
	}

	containersKnown := true
	if cl == nil {
		containersKnown = false
	} else if cs, err := cl.RunningBackupContainers(ctx); err != nil {
		maintenanceLog().Debug("cannot list backup containers", "error", err.Error())
		containersKnown = false
	} else {
		s.Running = append(s.Running, cs...)
	}

	sort.SliceStable(s.Running, func(i, j int) bool {
		if s.Running[i].StartedAt != s.Running[j].StartedAt {
			return s.Running[i].StartedAt < s.Running[j].StartedAt
		}
		return s.Running[i].ID < s.Running[j].ID
	})

	switch {
	case !containersKnown:
		s.Quiesce = QuiesceUnknown
	case len(s.Running) == 0:
		s.Quiesce = QuiesceQuiesced
	default:
		s.Quiesce = QuiesceBusy
	}
	return s, nil
}
