from .common import *  # NOQA
import pytest

namespace = {"client": None, "ns": None}


def test_daemonset():
    client = namespace["client"]
    ns = namespace["ns"]
    template = read_json_from_resource_dir("daemonset_1.json")
    name = random_name()
    # set name
    template["metadata"]["name"] = name
    # set namespace
    template["metadata"]["namespace"] = ns.id
    # set label and selector
    label_value = "apps.daemonset-{}-{}".format(ns.id, name)
    template["spec"]["template"]["metadata"]["labels"]["workload.user.cattle.io/workloadselector"] = label_value
    template["spec"]["selector"]["matchLabels"]["workload.user.cattle.io/workloadselector"] = label_value

    res = client.create_apps_daemonset(template)
    res = validate_daemonset(client, res)
    client.delete(res)


def get_worker_node(client):
    nodes = client.list_node(labelSelector="node-role.kubernetes.io/worker=true")
    return nodes.data


def validate_daemonset(client, daemonset):
    # wait for the deployment to be active
    wait_for(lambda: client.reload(daemonset).metadata.state.name == "active",
             timeout_message="time out waiting for deployment to be ready")
    res = client.reload(daemonset)
    name = res["metadata"]["name"]
    namespace = res["metadata"]["namespace"]
    node_count = len(get_worker_node(client))
    # Rancher Dashboard gets pods by passing the label selector
    pods = client.list_pod(
        labelSelector='workload.user.cattle.io/workloadselector=apps.daemonset-{}-{}'.format(namespace, name))
    assert "data" in pods.keys(), "failed to get pods"
    assert len(pods.data) == node_count, "failed to get the right number of pods"
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
