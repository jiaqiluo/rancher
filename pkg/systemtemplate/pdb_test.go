package systemtemplate

import (
	"strconv"
	"strings"
	"testing"

	apimgmtv3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

func pdbCluster(minAvailable, maxUnavailable string) *apimgmtv3.Cluster {
	return &apimgmtv3.Cluster{
		Spec: apimgmtv3.ClusterSpec{
			ClusterSpecBase: apimgmtv3.ClusterSpecBase{
				ClusterAgentDeploymentCustomization: &apimgmtv3.AgentDeploymentCustomization{
					SchedulingCustomization: &apimgmtv3.AgentSchedulingCustomization{
						PodDisruptionBudget: &apimgmtv3.PodDisruptionBudgetSpec{
							MinAvailable:   minAvailable,
							MaxUnavailable: maxUnavailable,
						},
					},
				},
			},
		},
	}
}

// TestPodDisruptionBudgetTemplateKeepsOutputForValidValues pins the exact text for the values that the
// webhook accepts. A change in this text can restart the cluster agent of existing clusters.
func TestPodDisruptionBudgetTemplateKeepsOutputForValidValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		minAvailable   string
		maxUnavailable string
		want           string
	}{
		{name: "min available integer", minAvailable: "1", want: "  minAvailable: 1\n"},
		{name: "min available percentage", minAvailable: "50%", want: "  minAvailable: 50%\n"},
		{name: "max unavailable integer", maxUnavailable: "2", want: "  maxUnavailable: 2\n"},
		{name: "max unavailable percentage", maxUnavailable: "100%", want: "  maxUnavailable: 100%\n"},
		{name: "both zero", minAvailable: "0", maxUnavailable: "0", want: "  maxUnavailable: 0\n"},
		{name: "largest integer", minAvailable: "2147483647", want: "  minAvailable: 2147483647\n"},
		{name: "zero percent", maxUnavailable: "0%", want: "  maxUnavailable: 0%\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out, err := PodDisruptionBudgetTemplate(pdbCluster(tt.minAvailable, tt.maxUnavailable))
			require.NoError(t, err)
			assert.Equal(t, "\napiVersion: policy/v1\nkind: PodDisruptionBudget\nmetadata:\n  name: cattle-cluster-agent-pod-disruption-budget\n  namespace: cattle-system\nspec:\n"+
				tt.want+"  selector:\n    matchLabels:\n      app: cattle-cluster-agent\n", string(out))
		})
	}
}

// TestPodDisruptionBudgetTemplateHostileValues checks that a value with YAML syntax stays one string
// and does not add keys or documents to the manifest that Rancher applies with cluster-admin rights.
func TestPodDisruptionBudgetTemplateHostileValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
	}{
		{name: "new line and new key", value: "1\n  paused: true"},
		{name: "new document", value: "1\n---\napiVersion: v1\nkind: Pod"},
		{name: "trailing new line", value: "50%\n"},
		{name: "double quote", value: `1"`},
		{name: "flow mapping", value: "{a: b}"},
		{name: "comment", value: "1 # x"},
		{name: "anchor", value: "&a 1"},
		{name: "tag", value: "!!str 1"},
		{name: "text", value: "abc"},
		{name: "negative integer", value: "-1"},
		{name: "integer above int32", value: "2147483648"},
		{name: "percentage above 100", value: "101%"},
		{name: "percentage above int32", value: "99999999999%"},
		{name: "plus sign", value: "+1"},
		{name: "fraction", value: "1.5"},
		{name: "percent sign only", value: "%"},
	}
	for _, tt := range tests {
		for _, field := range []string{"minAvailable", "maxUnavailable"} {
			t.Run(tt.name+" "+field, func(t *testing.T) {
				t.Parallel()
				cluster := pdbCluster(tt.value, "")
				if field == "maxUnavailable" {
					cluster = pdbCluster("", tt.value)
				}
				out, err := PodDisruptionBudgetTemplate(cluster)
				require.NoError(t, err)

				var obj struct {
					APIVersion string                 `json:"apiVersion"`
					Kind       string                 `json:"kind"`
					Metadata   map[string]interface{} `json:"metadata"`
					Spec       map[string]interface{} `json:"spec"`
				}
				require.NoError(t, yaml.UnmarshalStrict(out, &obj), string(out))
				assert.Equal(t, "PodDisruptionBudget", obj.Kind)
				assert.Len(t, obj.Spec, 2, string(out))
				assert.Equal(t, tt.value, obj.Spec[field])
				assert.Contains(t, obj.Spec, "selector")
			})
		}
	}
}

func TestIsValidPDBValue(t *testing.T) {
	t.Parallel()

	tests := []struct {
		value string
		want  bool
	}{
		{value: "0", want: true},
		{value: "1", want: true},
		{value: "2147483647", want: true},
		{value: "0%", want: true},
		{value: "50%", want: true},
		{value: "100%", want: true},
		{value: "", want: false},
		{value: "-1", want: false},
		{value: "+1", want: false},
		{value: "2147483648", want: false},
		{value: "101%", want: false},
		{value: "99999999999%", want: false},
		{value: "-1%", want: false},
		{value: "1.5", want: false},
		{value: "%", want: false},
		{value: "50%%", want: false},
		{value: "5 %", want: false},
		{value: "50%\n", want: false},
		{value: "abc", want: false},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, isValidPDBValue(tt.value), "value %q", tt.value)
	}
}

// kubernetesAcceptsPDBValue applies the rules of ValidatePositiveIntOrPercent and IsNotMoreThan100Percent in
// k8s.io/kubernetes/pkg/apis/apps/validation, which the API server uses for a PodDisruptionBudget.
// The repository does not import that package, so the test repeats its two small rules.
// An integer must not be negative. A percentage must match the Kubernetes format and must not be more than 100.
func kubernetesAcceptsPDBValue(value intstr.IntOrString) bool {
	if value.Type == intstr.Int {
		return value.IntValue() >= 0
	}
	if len(validation.IsValidPercent(value.StrVal)) != 0 {
		return false
	}
	percent, err := strconv.Atoi(strings.TrimSuffix(value.StrVal, "%"))
	return err == nil && percent <= 100
}

// TestIsValidPDBValueMatchesKubernetes checks that the rules of isValidPDBValue match the validation of
// the Kubernetes API server. The API server reads an unquoted number as an integer and any other value
// as a string, so the test decodes the rendered value into an IntOrString.
func TestIsValidPDBValueMatchesKubernetes(t *testing.T) {
	t.Parallel()

	for _, value := range []string{
		"0", "1", "2147483647", "2147483648", "-1", "+1", "1.5", "007",
		"0%", "50%", "100%", "101%", "-1%", "99999999999%", "%", "50%%", "5 %", "abc", "",
	} {
		var decoded intstr.IntOrString
		require.NoError(t, yaml.Unmarshal([]byte(pdbValue(value)), &decoded), "value %q", value)
		assert.Equal(t, isValidPDBValue(value), kubernetesAcceptsPDBValue(decoded),
			"value %q renders as %q", value, pdbValue(value))
	}
}
