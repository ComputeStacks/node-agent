package backup

import (
	"context"
	"cs-agent/containermgr"
	"cs-agent/types"
	"errors"
	"github.com/coreos/go-semver/semver"
	"math/rand"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
)

type MysqlInstance struct {
	Name         string
	Container    *containermgr.Container
	Version      *semver.Version
	BackupImage  string
	IPAddress    string
	Username     string
	Password     string
	Volume       types.Volume
	Calico       string // Calico token to enable network access
	ServiceID    string
	DeploymentID string
	Network      string
	Variant      string // mysql, mariadb, bitnami-mariadb
	DataPath     string // Where the data is located in the container
	MountPath    string // Where the volume is mounted. Used to locate the correct volume.
	User         string // The Linux user who should own the data
	Group        string // Linux group who should own th data
}

func loadMysqlMaster(cli *client.Client, serviceID string, event *progress, allowOff bool) (*MysqlInstance, error) {
	ctx := context.Background()
	mysqlFoundContainer, err := containermgr.FindByService(cli, serviceID, allowOff)

	if err != nil {
		backupLogger().Warn("Failed to load MySQL Master", "error", err.Error(), "function", "loadMysqlMaster", "action", "FindRunningContainer", "serviceID", serviceID)
		if event != nil {
			event.PostEventUpdate("agent-6e0d0cbb268ac0b1", "Container appears to be offline. Unable to perform backup.")
			event.EventLog.Status = "cancelled"
		}
		return &MysqlInstance{}, err
	}

	// Set defaults
	instance := MysqlInstance{
		Container: mysqlFoundContainer,
		DataPath:  "/var/lib/mysql",
		MountPath: "/var/lib/mysql",
		User:      "mysql",
		Group:     "mysql",
	}

	mysqlContainer, err := cli.ContainerInspect(ctx, mysqlFoundContainer.ID)

	if err != nil {
		backupLogger().Warn("Failed to load MySQL Master", "error", err.Error(), "function", "loadMysqlMaster", "action", "ContainerInspect")
		if event != nil {
			event.PostEventUpdate("agent-mysql-inspect-failed", "Unable to inspect the database container: "+err.Error())
		}
		return &instance, err
	}

	versionStage := ""

	for _, c := range mysqlContainer.Config.Env {
		keys := strings.Split(c, "=")
		switch keys[0] {
		case "MYSQL_ROOT_PASSWORD":
			instance.Username = "root"
			instance.Password = keys[1]
		case "MYSQL_MAJOR":
			versionStage = keys[1]
			instance.Variant = "mysql"
		case "MARIADB_MAJOR":
			versionStage = keys[1]
			instance.Variant = "mariadb"
		case "MARIADB_VERSION":
			instance.Variant = "mariadb"
		case "MYSQL_USER":
			instance.Username = keys[1]
		case "MYSQL_PASSWORD":
			instance.Password = keys[1]
		case "BITNAMI_APP_NAME":
			if keys[1] == "mariadb" {
				instance.Variant = "bitnami-mariadb"
				instance.DataPath = "/bitnami/mariadb/data"
				instance.MountPath = "/bitnami/mariadb"
				instance.User = "1001"
				instance.Group = "root"
			}
		case "BITNAMI_IMAGE_VERSION":
			versionStage = keys[1]
		case "MARIADB_ROOT_PASSWORD": // Bitnami
			instance.Username = "root"
			instance.Password = keys[1]
		}
	}

	// Ensure we load up all our service IDs to allow our backup container full access to the project.
	// Also grab the MariaDB version that uses labels rather than env vars.
	for k, v := range mysqlContainer.Config.Labels {
		switch k {
		case "org.projectcalico.label.token":
			instance.Calico = v
		case "com.computestacks.deployment_id":
			instance.DeploymentID = v
		case "com.computestacks.service_id":
			instance.ServiceID = v
		case "org.opencontainers.image.version":
			versionStage = v
		}
	}

	if versionStage == "" {
		backupLogger().Warn("Failed to identify MySQL Version", "error", "version string is blank", "function", "loadMysqlMaster", "serviceID", serviceID)
		if event != nil {
			event.PostEventUpdate("agent-f422717152297b23", "Unable to load MySQL Version, halting job.")
			event.EventLog.Status = "failed"
		}
		return &instance, errors.New("missing version string")
	}

	// format version string
	dotParts := strings.SplitN(versionStage, ".", 3)

	// Convert to dotted-tri format
	switch len(dotParts) {
	case 1:
		versionStage = versionStage + ".0.0" // 1 => 1.0.0
	case 2:
		versionStage = versionStage + ".0" // 1.0 => 1.0.0
	}

	v, vErr := semver.NewVersion(versionStage)
	if vErr != nil {
		backupLogger().Warn("Failed to identify MySQL Version", "error", vErr.Error(), "function", "loadMysqlMaster", "serviceID", serviceID)
		if event != nil {
			event.PostEventUpdate("agent-114697045cd756da", "Unable to load MySQL Version, halting job.\n\n"+vErr.Error())
			event.EventLog.Status = "failed"
		}
		return &instance, vErr
	}

	instance.Version = v

	for _, v := range mysqlContainer.Mounts {
		if v.Destination == instance.MountPath {
			instance.Volume = types.Volume{Name: v.Name}
		}
	}

	var ipAddr string

	for name, net := range mysqlContainer.NetworkSettings.Networks {
		if net.IPAddress != "" {
			ipAddr = net.IPAddress
			instance.Network = name
			break
		}
	}

	if ipAddr == "" && !allowOff {
		backupLogger().Warn("Failed to load mySQLMaster IP Address", "serviceID", serviceID, "container", mysqlContainer.Name)
		reason := "Unable to find an IP address for database container " + strings.TrimPrefix(mysqlContainer.Name, "/") +
			": docker reports no attached network endpoint"
		if event != nil {
			event.PostEventUpdate("agent-mysql-no-ip", reason)
		}
		return &instance, errors.New(reason)
	} else if ipAddr != "" {
		instance.IPAddress = ipAddr
	}

	// Set Image
	if instance.Variant == "mysql" {
		if instance.Version.LessThan(semver.Version{Major: int64(8), Minor: int64(0)}) {
			instance.BackupImage = "ghcr.io/computestacks/cs-docker-xtrabackup:2.4"
		} else {
			instance.BackupImage = "ghcr.io/computestacks/cs-docker-xtrabackup:8.0"
		}
	} else { // both mariadb and bitnami-mariadb
		instance.BackupImage = ""
	}

	backupLogger().Info("Have MySQL Target", "variant", instance.Variant, "version", instance.Version.String(), "image", instance.BackupImage)

	return &instance, nil
}

func buildBackupAgent(cli *client.Client, mysqlMaster *MysqlInstance) (*containermgr.Container, error) {

	ctx := context.Background()

	backupLogger().Debug("Building backup container for SQL server", "variant", mysqlMaster.Variant, "version", mysqlMaster.Version, "image", mysqlMaster.BackupImage)

	// Ensure image exists
	_, _, missingImage := cli.ImageInspectWithRaw(ctx, mysqlMaster.BackupImage)

	if missingImage != nil {
		_, err := cli.ImagePull(ctx, mysqlMaster.BackupImage, image.PullOptions{})
		if err != nil {
			return nil, err
		}
		// Looks like it's moving on too quickly, wait 1 second to make sure image is available.
		time.Sleep(time.Second)
	}

	labels := make(map[string]string)
	if mysqlMaster.Calico != "" {
		labels["org.projectcalico.label.token"] = mysqlMaster.Calico
	}
	if mysqlMaster.DeploymentID != "" {
		labels["com.computestacks.deployment_id"] = mysqlMaster.DeploymentID
	}
	if mysqlMaster.ServiceID != "" {
		labels["com.computestacks.service_id"] = mysqlMaster.ServiceID
	}
	labels["com.computestacks.role"] = "backup"

	backupLogger().Debug("Master Params", "calico", mysqlMaster.Calico, "deployment", mysqlMaster.DeploymentID, "service", mysqlMaster.ServiceID)

	t := time.Now()

	if mysqlMaster.Volume.IsEmpty() {
		backupLogger().Debug("MySQL Master volume is nil")
		return nil, errors.New("database master has no volume to mount")
	}
	backupLogger().Debug("Have MySQL Volume", "volume", mysqlMaster.Volume.Name)

	env := []string{"MYSQL_HOST=" + mysqlMaster.IPAddress, "MYSQL_PASSWORD=" + mysqlMaster.Password} // Used by init script to ensure MySQL is available before attempting a backup

	rand.New(rand.NewSource(time.Now().UnixNano())) // Seed for random container name
	randNumber := 10 + rand.Intn(1000-10)
	containerName := "backup-" + strconv.Itoa(randNumber) + string(t.Format("150405"))

	resp, err := cli.ContainerCreate(ctx, &container.Config{
		Image:  mysqlMaster.BackupImage,
		Labels: labels,
		Env:    env,
	}, &container.HostConfig{
		NetworkMode: container.NetworkMode(mysqlMaster.Network),
		Mounts: []mount.Mount{
			{
				Type:   mount.TypeVolume,
				Source: mysqlMaster.Volume.Name,
				Target: mysqlMaster.MountPath,
			},
		},
		AutoRemove: true,
	}, nil, nil, containerName)
	if err != nil {
		return nil, err
	}

	if err := cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		return nil, err
	}

	time.Sleep(2 * time.Second)

	return &containermgr.Container{ID: resp.ID}, nil
}
