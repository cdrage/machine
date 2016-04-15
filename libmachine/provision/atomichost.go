package provision

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"

	"github.com/docker/machine/libmachine/auth"
	"github.com/docker/machine/libmachine/drivers"
	"github.com/docker/machine/libmachine/engine"
	"github.com/docker/machine/libmachine/log"
	"github.com/docker/machine/libmachine/provision/pkgaction"
	"github.com/docker/machine/libmachine/provision/serviceaction"
	"github.com/docker/machine/libmachine/swarm"
)

func init() {
	Register("AtomicHost", &RegisteredProvisioner{
		New: func(d drivers.Driver) Provisioner {
			return NewAtomicHostProvisioner("atomic.host", d)
		},
	})
}

func NewAtomicHostProvisioner(osReleaseID string, d drivers.Driver) *AtomicHostProvisioner {
	systemdProvisioner := NewSystemdProvisioner(osReleaseID, d)
	systemdProvisioner.SSHCommander = RedHatSSHCommander{Driver: d}
	return &AtomicHostProvisioner{
		systemdProvisioner,
	}
}

type AtomicHostProvisioner struct {
	SystemdProvisioner
}

func (provisioner *AtomicHostProvisioner) String() string {
	return "atomic.host"
}

func (provisioner *AtomicHostProvisioner) GenerateDockerOptions(dockerPort int) (*DockerOptions, error) {
	var (
		engineCfg bytes.Buffer
	)

	driverNameLabel := fmt.Sprintf("provider=%s", provisioner.Driver.DriverName())
	provisioner.EngineOptions.Labels = append(provisioner.EngineOptions.Labels, driverNameLabel)

	engineConfigTmpl := `[Unit]
Description=Docker Application Container Engine
Documentation=http://docs.docker.com
After=network.target

[Service]
ExecStart=/usr/bin/docker -d -H tcp://0.0.0.0:{{.DockerPort}} -H unix:///var/run/docker.sock --storage-driver {{.EngineOptions.StorageDriver}} --tlsverify --tlscacert {{.AuthOptions.CaCertRemotePath}} --tlscert {{.AuthOptions.ServerCertRemotePath}} --tlskey {{.AuthOptions.ServerKeyRemotePath}} {{ range .EngineOptions.Labels }}--label {{.}} {{ end }}{{ range .EngineOptions.InsecureRegistry }}--insecure-registry {{.}} {{ end }}{{ range .EngineOptions.RegistryMirror }}--registry-mirror {{.}} {{ end }}{{ range .EngineOptions.ArbitraryFlags }}--{{.}} {{ end }}
MountFlags=slave
LimitNOFILE=1048576
LimitNPROC=1048576
LimitCORE=infinity
Environment={{range .EngineOptions.Env}}{{ printf "%q" . }} {{end}}

[Install]
WantedBy=multi-user.target
`
	t, err := template.New("engineConfig").Parse(engineConfigTmpl)
	if err != nil {
		return nil, err
	}

	engineConfigContext := EngineConfigContext{
		DockerPort:    dockerPort,
		AuthOptions:   provisioner.AuthOptions,
		EngineOptions: provisioner.EngineOptions,
	}

	t.Execute(&engineCfg, engineConfigContext)

	log.Debug(provisioner.DaemonOptionsFile)
	return &DockerOptions{
		EngineOptions:     engineCfg.String(),
		EngineOptionsPath: provisioner.DaemonOptionsFile,
	}, nil
}

func (provisioner *AtomicHostProvisioner) Service(name string, action serviceaction.ServiceAction) error {
	reloadDaemon := false
	switch action {
	case serviceaction.Start, serviceaction.Restart:
		reloadDaemon = true
	}

	// similar to the suse.go provider, systemd needs to be reloaded when config
	// changes are made on disk.
	if reloadDaemon {
		if _, err := provisioner.SSHCommand("sudo systemctl daemon-reload"); err != nil {
			return err
		}
	}

	command := fmt.Sprintf("sudo systemctl %s %s", action.String(), name)

	if _, err := provisioner.SSHCommand(command); err != nil {
		return err
	}

	return nil
}

func (provisioner *AtomicHostProvisioner) Package(name string, action pkgaction.PackageAction) error {

	if name == "docker" && action == pkgaction.Upgrade {
		return provisioner.upgrade()
	}

	return nil
}

func (provisioner *AtomicHostProvisioner) Provision(swarmOptions swarm.Options, authOptions auth.Options, engineOptions engine.Options) error {
	provisioner.SwarmOptions = swarmOptions
	provisioner.AuthOptions = authOptions
	provisioner.EngineOptions = engineOptions
	swarmOptions.Env = engineOptions.Env

	if provisioner.EngineOptions.StorageDriver == "" {
		provisioner.EngineOptions.StorageDriver = "overlay"
	} else if provisioner.EngineOptions.StorageDriver != "overlay" {
		return fmt.Errorf("Unsupported storage driver: %s", provisioner.EngineOptions.StorageDriver)
	}

	log.Debugf("Setting hostname %s", provisioner.Driver.GetMachineName())
	if err := provisioner.SetHostname(provisioner.Driver.GetMachineName()); err != nil {
		return err
	}

	log.Debugf("Make daemon options dir")
	if err := makeDockerOptionsDir(provisioner); err != nil {
		return err
	}

	log.Debugf("Preparing certificates")
	provisioner.AuthOptions = setRemoteAuthOptions(provisioner)

	log.Debugf("Setting up certificates")
	if err := ConfigureAuth(provisioner); err != nil {
		log.Debugf("Certs screwed up")
		return err
	}

	log.Debugf("Configuring swarm")
	if err := configureSwarm(provisioner, swarmOptions, provisioner.AuthOptions); err != nil {
		return err
	}

	return nil
}

func (provisioner *AtomicHostProvisioner) upgrade() error {
	log.Infof("Running 'atomic host upgrade' (this may take a while)...")

	// Only reboots if there is a upgrade available
	upgradeCommandOutput, err := provisioner.SSHCommand("sudo atomic host upgrade")
	if err != nil {
		switch err.Error() {
		// See https://github.com/projectatomic/rpm-ostree/blob/master/man/rpm-ostree.xml
		// for error code. Return nil as CentOS 7 still uses older version of rpm-ostree
		case "exit status 77":
			log.Infof("No upgrade available at this time.")
			return nil
		default:
			return err
		}
	}

	// Still parse SSH output for 'no upgrade available' due to older versions of
	// rpm-ostree where exit code 77 is not yet implemented
	if strings.Contains(upgradeCommandOutput, "No upgrade available.") {
		log.Infof("No upgrade available at this time.")
	} else {
		log.Infof("Upgrade succeeded, rebooting.")
		// ignore errors here because the SSH connection will close
		provisioner.SSHCommand("sudo reboot")
	}

	return nil
}
