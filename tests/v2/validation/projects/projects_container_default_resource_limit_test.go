//go:build (validation || infra.any || cluster.any || extended) && !sanity && !stress

package projects

import (
	"fmt"
	"testing"

	"github.com/rancher/rancher/tests/v2/actions/kubeapi/namespaces"
	"github.com/rancher/rancher/tests/v2/actions/kubeapi/projects"
	"github.com/rancher/rancher/tests/v2/actions/rbac"
	deployment "github.com/rancher/rancher/tests/v2/actions/workloads/deployment"
	"github.com/rancher/shepherd/clients/rancher"
	management "github.com/rancher/shepherd/clients/rancher/generated/management/v3"
	"github.com/rancher/shepherd/extensions/charts"
	"github.com/rancher/shepherd/extensions/clusters"
	"github.com/rancher/shepherd/extensions/users"
	"github.com/rancher/shepherd/pkg/session"
	"github.com/rancher/shepherd/pkg/wrangler"
	log "github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type ProjectsContainerResourceLimitTestSuite struct {
	suite.Suite
	client  *rancher.Client
	session *session.Session
	cluster *management.Cluster
}

func (pcrl *ProjectsContainerResourceLimitTestSuite) TearDownSuite() {
	pcrl.session.Cleanup()
}

func (pcrl *ProjectsContainerResourceLimitTestSuite) SetupSuite() {
	pcrl.session = session.NewSession()

	client, err := rancher.NewClient("", pcrl.session)
	require.NoError(pcrl.T(), err)

	pcrl.client = client

	clusterName := client.RancherConfig.ClusterName
	require.NotEmptyf(pcrl.T(), clusterName, "Cluster name to install should be set")
	clusterID, err := clusters.GetClusterIDByName(pcrl.client, clusterName)
	require.NoError(pcrl.T(), err, "Error getting cluster ID")
	pcrl.cluster, err = pcrl.client.Management.Cluster.ByID(clusterID)
	require.NoError(pcrl.T(), err)
}

func (pcrl *ProjectsContainerResourceLimitTestSuite) setupUserForProject() (*rancher.Client, *wrangler.Context) {
	log.Info("Create a standard user and add the user to the downstream cluster as cluster owner.")
	standardUser, err := users.CreateUserWithRole(pcrl.client, users.UserConfig(), projects.StandardUser)
	require.NoError(pcrl.T(), err, "Failed to create standard user")
	standardUserClient, err := pcrl.client.AsUser(standardUser)
	require.NoError(pcrl.T(), err)
	err = users.AddClusterRoleToUser(pcrl.client, pcrl.cluster, standardUser, rbac.ClusterOwner.String(), nil)
	require.NoError(pcrl.T(), err, "Failed to add the user as a cluster owner to the downstream cluster")

	standardUserContext, err := standardUserClient.WranglerContext.DownStreamClusterWranglerContext(pcrl.cluster.ID)
	require.NoError(pcrl.T(), err)

	return standardUserClient, standardUserContext
}

func (pcrl *ProjectsContainerResourceLimitTestSuite) TestLimitDeletionPropagationToExistingNamespaces() {
	subSession := pcrl.session.NewSession()
	defer subSession.Cleanup()

	standardUserClient, _ := pcrl.setupUserForProject()

	log.Info("Create a project (with container default resource limit) and a namespace in the project.")
	cpuLimit := "100m"
	cpuReservation := "50m"
	memoryLimit := "64Mi"
	memoryReservation := "32Mi"

	createdProject, createdNamespace, err := createProjectAndNamespaceWithLimits(standardUserClient, pcrl.cluster.ID, cpuLimit, cpuReservation, memoryLimit, memoryReservation)
	require.NoError(pcrl.T(), err)

	log.Info("Verify that the container default resource limit in the Project spec is accurate.")
	projectSpec := createdProject.Spec.ContainerDefaultResourceLimit
	require.Equal(pcrl.T(), cpuLimit, projectSpec.LimitsCPU, "CPU limit mismatch")
	require.Equal(pcrl.T(), cpuReservation, projectSpec.RequestsCPU, "CPU reservation mismatch")
	require.Equal(pcrl.T(), memoryLimit, projectSpec.LimitsMemory, "Memory limit mismatch")
	require.Equal(pcrl.T(), memoryReservation, projectSpec.RequestsMemory, "Memory reservation mismatch")

	log.Info("Verify that the namespace has the label and annotation referencing the project.")
	updatedNamespace, err := namespaces.GetNamespaceByName(standardUserClient, pcrl.cluster.ID, createdNamespace.Name)
	require.NoError(pcrl.T(), err)
	err = checkNamespaceLabelsAndAnnotations(pcrl.cluster.ID, createdProject.Name, updatedNamespace)
	require.NoError(pcrl.T(), err)

	log.Info("Verify that the limit range object is created for the namespace and the resource limit in the limit range is accurate.")
	err = checkLimitRange(standardUserClient, pcrl.cluster.ID, updatedNamespace.Name, cpuLimit, cpuReservation, memoryLimit, memoryReservation)
	require.NoError(pcrl.T(), err)

	log.Info("Remove the container default limits set in the Project.")
	cpuLimit = ""
	cpuReservation = ""
	memoryLimit = ""
	memoryReservation = ""
	updatedProject, err := updateProjectContainerResourceLimit(standardUserClient, createdProject, cpuLimit, cpuReservation, memoryLimit, memoryReservation)
	require.NoError(pcrl.T(), err, "Failed to update container resource limit.")

	log.Info("Verify that the container default resource limits in the Project spec has been updated.")
	projectSpec = updatedProject.Spec.ContainerDefaultResourceLimit
	require.Equal(pcrl.T(), cpuLimit, projectSpec.LimitsCPU, "CPU limit mismatch")
	require.Equal(pcrl.T(), cpuReservation, projectSpec.RequestsCPU, "CPU reservation mismatch")
	require.Equal(pcrl.T(), memoryLimit, projectSpec.LimitsMemory, "Memory limit mismatch")
	require.Equal(pcrl.T(), memoryReservation, projectSpec.RequestsMemory, "Memory reservation mismatch")

	log.Info("Verify that the limit range in the existing namespace is deleted.")
	ctx, err := standardUserClient.WranglerContext.DownStreamClusterWranglerContext(pcrl.cluster.ID)
	limitRanges, err := ctx.Core.LimitRange().List(updatedNamespace.Name, metav1.ListOptions{})
	require.NoError(pcrl.T(), err)
	require.Equal(pcrl.T(), 0, len(limitRanges.Items))

	log.Info("Create a deployment in the namespace with one replica and verify that a pod is created.")
	createdDeployment, err := deployment.CreateDeployment(standardUserClient, pcrl.cluster.ID, updatedNamespace.Name, 1, "", "", false, false)
	require.NoError(pcrl.T(), err, "Failed to create deployment in the namespace")
	err = charts.WatchAndWaitDeployments(standardUserClient, pcrl.cluster.ID, updatedNamespace.Name, metav1.ListOptions{
		FieldSelector: "metadata.name=" + createdDeployment.Name,
	})
	require.NoError(pcrl.T(), err)

	log.Info("Verify that the resource limits and requests for the container in the pod spec is accurate.")
	err = checkContainerResources(standardUserClient, pcrl.cluster.ID, updatedNamespace.Name, createdDeployment.Name, cpuLimit, cpuReservation, memoryLimit, memoryReservation)
	require.NoError(pcrl.T(), err)
}

func (pcrl *ProjectsContainerResourceLimitTestSuite) TestOverrideDefaultLimitInNamespace() {
	subSession := pcrl.session.NewSession()
	defer subSession.Cleanup()

	standardUserClient, standardUserContext := pcrl.setupUserForProject()

	log.Info("Create a project (with container default resource limit) and a namespace in the project.")
	cpuLimit := "100m"
	cpuReservation := "50m"
	memoryLimit := "64Mi"
	memoryReservation := "32Mi"

	createdProject, createdNamespace, err := createProjectAndNamespaceWithLimits(standardUserClient, pcrl.cluster.ID, cpuLimit, cpuReservation, memoryLimit, memoryReservation)
	require.NoError(pcrl.T(), err)

	log.Info("Verify that the container default resource limit in the Project spec is accurate.")
	projectSpec := createdProject.Spec.ContainerDefaultResourceLimit
	require.Equal(pcrl.T(), cpuLimit, projectSpec.LimitsCPU, "CPU limit mismatch")
	require.Equal(pcrl.T(), cpuReservation, projectSpec.RequestsCPU, "CPU reservation mismatch")
	require.Equal(pcrl.T(), memoryLimit, projectSpec.LimitsMemory, "Memory limit mismatch")
	require.Equal(pcrl.T(), memoryReservation, projectSpec.RequestsMemory, "Memory reservation mismatch")

	log.Info("Verify that the namespace has the label and annotation referencing the project.")
	updatedNamespace, err := namespaces.GetNamespaceByName(standardUserClient, pcrl.cluster.ID, createdNamespace.Name)
	require.NoError(pcrl.T(), err)
	err = checkNamespaceLabelsAndAnnotations(pcrl.cluster.ID, createdProject.Name, updatedNamespace)
	require.NoError(pcrl.T(), err)

	log.Info("Verify that the limit range object is created for the namespace and the resource limit in the limit range is accurate.")
	err = checkLimitRange(standardUserClient, pcrl.cluster.ID, updatedNamespace.Name, cpuLimit, cpuReservation, memoryLimit, memoryReservation)
	require.NoError(pcrl.T(), err)

	log.Info("Create a deployment in the namespace with one replica and verify that a pod is created.")
	createdDeployment, err := deployment.CreateDeployment(standardUserClient, pcrl.cluster.ID, updatedNamespace.Name, 1, "", "", false, false)
	require.NoError(pcrl.T(), err, "Failed to create deployment in the namespace")
	err = charts.WatchAndWaitDeployments(standardUserClient, pcrl.cluster.ID, updatedNamespace.Name, metav1.ListOptions{
		FieldSelector: "metadata.name=" + createdDeployment.Name,
	})
	require.NoError(pcrl.T(), err)

	log.Info("Verify that the resource limits and requests for the container in the pod spec is accurate.")
	err = checkContainerResources(standardUserClient, pcrl.cluster.ID, updatedNamespace.Name, createdDeployment.Name, cpuLimit, cpuReservation, memoryLimit, memoryReservation)
	require.NoError(pcrl.T(), err)

	log.Info("Override the CPU, memory limit and request in the namespace.")
	cpuLimit = "150m"
	cpuReservation = "100m"
	memoryLimit = "128Mi"
	memoryReservation = "64Mi"
	if _, exists := updatedNamespace.Annotations[containerDefaultLimitAnnotation]; !exists {
		updatedNamespace.Annotations[containerDefaultLimitAnnotation] = fmt.Sprintf(`{"limitsCpu":"%s","limitsMemory":"%s","requestsCpu":"%s","requestsMemory":"%s"}`, cpuLimit, memoryLimit, cpuReservation, memoryReservation)
	}

	currentNamespace, err := namespaces.GetNamespaceByName(standardUserClient, pcrl.cluster.ID, updatedNamespace.Name)
	require.NoError(pcrl.T(), err)
	updatedNamespace.ResourceVersion = currentNamespace.ResourceVersion
	namespace, err := standardUserContext.Core.Namespace().Update(updatedNamespace)
	require.NoError(pcrl.T(), err)

	log.Info("Verify that the resource limit in the limit range is accurate.")
	err = checkLimitRange(standardUserClient, pcrl.cluster.ID, namespace.Name, cpuLimit, cpuReservation, memoryLimit, memoryReservation)
	require.NoError(pcrl.T(), err)

	log.Info("Create a deployment in the namespace with one replica and verify that a pod is created.")
	createdDeployment, err = deployment.CreateDeployment(standardUserClient, pcrl.cluster.ID, updatedNamespace.Name, 1, "", "", false, false)
	require.NoError(pcrl.T(), err, "Failed to create deployment in the namespace")
	err = charts.WatchAndWaitDeployments(standardUserClient, pcrl.cluster.ID, namespace.Name, metav1.ListOptions{
		FieldSelector: "metadata.name=" + createdDeployment.Name,
	})
	require.NoError(pcrl.T(), err)

	log.Info("Verify that the resource limits and requests for the container in the pod spec is accurate.")
	err = checkContainerResources(standardUserClient, pcrl.cluster.ID, namespace.Name, createdDeployment.Name, cpuLimit, cpuReservation, memoryLimit, memoryReservation)
	require.NoError(pcrl.T(), err)
}

func TestProjectsContainerResourceLimitTestSuite(t *testing.T) {
	suite.Run(t, new(ProjectsContainerResourceLimitTestSuite))
}