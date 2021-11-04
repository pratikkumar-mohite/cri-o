package container

import (
	"github.com/cri-o/cri-o/internal/config/nsmgr"
	"github.com/cri-o/cri-o/internal/lib/sandbox"
	rspec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/runtime-tools/generate"
	"github.com/pkg/errors"
	types "k8s.io/cri-api/pkg/apis/runtime/v1"
)

func (c *container) SpecAddNamespaces(sb *sandbox.Sandbox) error {
	// Join the namespace paths for the pod sandbox container.
	if err := ConfigureGeneratorGivenNamespacePaths(sb.NamespacePaths(), &c.spec); err != nil {
		return errors.Wrap(err, "failed to configure namespaces in container create")
	}

	sc := c.config.Linux.SecurityContext

	if sc.NamespaceOptions.Network == types.NamespaceMode_NODE {
		if err := c.spec.RemoveLinuxNamespace(string(rspec.NetworkNamespace)); err != nil {
			return err
		}
	}

	switch sc.NamespaceOptions.Pid {
	case types.NamespaceMode_NODE:
		// kubernetes PodSpec specify to use Host PID namespace
		if err := c.spec.RemoveLinuxNamespace(string(rspec.PIDNamespace)); err != nil {
			return err
		}
	case types.NamespaceMode_POD:
		pidNsPath := sb.PidNsPath()
		if pidNsPath == "" {
			if sb.NamespaceOptions().Pid != types.NamespaceMode_POD {
				return errors.New("Pod level PID namespace requested for the container, but pod sandbox was not similarly configured, and does not have an infra container")
			}
			return errors.New("PID namespace requested, but sandbox infra container unexpectedly invalid")
		}

		if err := c.spec.AddOrReplaceLinuxNamespace(string(rspec.PIDNamespace), pidNsPath); err != nil {
			return errors.Wrapf(err, "updating container PID namespace to pod")
		}
	}
	return nil
}

// ConfigureGeneratorGivenNamespacePaths takes a map of nsType -> nsPath. It configures the generator
// to add or replace the defaults to these paths
func ConfigureGeneratorGivenNamespacePaths(managedNamespaces []*sandbox.ManagedNamespace, g *generate.Generator) error {
	typeToSpec := map[nsmgr.NSType]rspec.LinuxNamespaceType{
		nsmgr.IPCNS:  rspec.IPCNamespace,
		nsmgr.NETNS:  rspec.NetworkNamespace,
		nsmgr.UTSNS:  rspec.UTSNamespace,
		nsmgr.USERNS: rspec.UserNamespace,
	}

	for _, ns := range managedNamespaces {
		// allow for empty paths, as this namespace just shouldn't be configured
		if ns.Path() == "" {
			continue
		}
		nsForSpec := typeToSpec[ns.Type()]
		if nsForSpec == "" {
			return errors.Errorf("Invalid namespace type %s", ns.Type())
		}
		if err := g.AddOrReplaceLinuxNamespace(string(nsForSpec), ns.Path()); err != nil {
			return err
		}
	}
	return nil
}
