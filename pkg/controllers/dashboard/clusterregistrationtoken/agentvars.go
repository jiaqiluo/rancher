package clusterregistrationtoken

import (
	"fmt"
	"strings"

	v3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	"github.com/rancher/rancher/pkg/capr"
	"github.com/sirupsen/logrus"
)

type EnvType int

const (
	Linux EnvType = iota
	PowerShell
	Docker
)

// AgentEnvVars renders the agent environment variables of the cluster for the node registration commands.
// The user copies and runs these commands, so each value is quoted and the shell cannot parse it as syntax.
// A variable with a name that is not a valid identifier is skipped.
func AgentEnvVars(cluster *v3.Cluster, envType EnvType) string {
	var agentEnvVars []string
	if cluster == nil {
		return ""
	}
	for _, envVar := range cluster.Spec.AgentEnvVars {
		if envVar.Value == "" {
			continue
		}
		if !capr.IsValidEnvVarName(envVar.Name) {
			logrus.Warnf("skipping agent environment variable with invalid name: %q", envVar.Name)
			continue
		}
		switch envType {
		case Docker:
			agentEnvVars = append(agentEnvVars, fmt.Sprintf("-e %s", capr.ShellQuoteValue(envVar.Name+"="+envVar.Value)))
		case PowerShell:
			agentEnvVars = append(agentEnvVars, fmt.Sprintf("$env:%s=%s;", envVar.Name, capr.PowerShellQuoteValue(envVar.Value)))
		default:
			agentEnvVars = append(agentEnvVars, fmt.Sprintf("%s=%s", envVar.Name, capr.ShellQuoteValue(envVar.Value)))
		}
	}
	return strings.Join(agentEnvVars, " ")
}
