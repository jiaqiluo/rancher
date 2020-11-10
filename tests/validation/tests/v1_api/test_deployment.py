from .common import *  # NOQA
import pytest

namespace = {"client": None, "ns": None}


def test_namespace_create():
    t = {
        "type": "namespace",
        "metadata": {
            "name": random_test_name("test-ns")
        }
    }
    client = namespace["client"]
    res = client.create_namespace(t)
    # validate the namespace is created
    ns = client.by_id_namespace(res.id)
    assert ns.id == res.id
    # delete the namespace at the end
    client.delete(ns)


def test_deployment():
    client = namespace["client"]
    ns = namespace["ns"]
    template = read_json_from_resource_dir("deployment_1.json")
    name = random_name()
    # set name
    template["metadata"]["name"] = name
    # set namespace
    template["metadata"]["namespace"] = ns.id
    # set label and selector
    label_value = "apps.deployment-{}-{}".format(ns.id, name)
    template["spec"]["template"]["metadata"]["labels"]["workload.user.cattle.io/workloadselector"] = label_value
    template["spec"]["selector"]["matchLabels"]["workload.user.cattle.io/workloadselector"] = label_value

    deployment = client.create_apps_deployment(template)
    deployment = validate_deployment(client, deployment)
    client.delete(deployment)


def validate_deployment(client, deployment):
    # wait for the deployment to be active
    wait_for(lambda: client.reload(deployment).metadata.state.name == "active",
             timeout_message="time out waiting for deployment to be ready")
    res = client.reload(deployment)
    name = res["metadata"]["name"]
    namespace = res["metadata"]["namespace"]
    replicas = res["spec"]["replicas"]
    # Rancher Dashboard gets pods by passing the label selector
    pods = client.list_pod(
        labelSelector='workload.user.cattle.io/workloadselector=apps.deployment-{}-{}'.format(namespace, name))
    assert "data" in pods.keys(), "failed to get pods"
    assert len(pods.data) == replicas, "failed to get the right number of pods"
    for pod in pods.data:
        assert pod.metadata.state.name == "running"
    return res


@pytest.fixture(scope='module', autouse="True")
def create_client(request):
    client = get_cluster_client_for_token_v1()
    t = {
        "type": "namespace",
        "metadata": {
            "name": random_test_name()
        }
    }
    ns = client.create_namespace(t)

    namespace["client"] = client
    namespace["ns"] = ns

    def fin():
        client.delete(namespace["ns"])

    request.addfinalizer(fin)
