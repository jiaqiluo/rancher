from tests.v3_api.common import *  # NOQA
import urllib


rancher_monitoring_v2 = {
   "type": "chartInstallAction",
   "disableOpenAPIValidation": False,
   "noHooks": False,
   "skipCRDs": False,
   "wait": True,
   "charts": [
      {
         "chartName": "rancher-monitoring-crd",
         "version": "8.16.10000",
         "releaseName": "rancher-monitoring-crd",
         "namespace": "monitoring-system"
      },
      {
         "chartName": "rancher-monitoring",
         "version": "8.16.100",
         "releaseName": "rancher-monitoring",
         "namespace": "monitoring-system",
         "values": {}
      }
   ]
}


def test_install_monitoring_v2():
    client = get_admin_client_v1()
    a = client.list_catalog_cattle_io_repo()
    print(a.data)
    rancher_catalog = client.by_id_catalog_cattle_io_repo(id="default/v1")
    # rancher_catalog = client.by_id_catalog_cattle_io_repo(id="default/rancher-charts")
    print(rancher_catalog)
    print("----------")
    if rancher_catalog is None:
        assert False, "rancher-charts is not available"
    res = client.action(rancher_catalog, "install",
                        rancher_monitoring_v2)
    print(res)
    time.sleep(10)
    ns = "dashboard-catalog"
    pod = res.get("operationName")
    container = "helm"
    valiadate_helm_operator_log(ns, pod, container)


def valiadate_helm_operator_log(namespace, pod, container):
    url_base = 'wss://' + CATTLE_TEST_URL[8:] + \
               '/api/v1/namespaces/' + namespace + \
               '/pods/' + pod + \
               '/log?'
    params_dict = {
        "container": container,
        "follow": True,
        "timestamps": True,
        "previous": False,
        "pretty": True
    }
    params = urllib.parse.urlencode(params_dict, doseq=True,
                                    quote_via=urllib.parse.quote, safe='()')
    url = url_base + params
    print("url is {}".format(url))
    ws = create_connection(url, ["base64.binary.k8s.io"])
    logparse = WebsocketLogParse()
    logparse.start_thread(target=logparse.receiver, args=(ws, False))

    print('\noutput:\n' + logparse.last_message + '\n')
    assert 'Error' not in logparse.last_message, \
        "failed to install the monitoring app"
    logparse.last_message = ''

    ws.close()






def test_catalog_cattle_io_repo():
    client = get_admin_client_v1()
    # for item in dir(client):
    #     print(item)
    res = client.list_catalog_cattle_io_clusterrepo()
    print("count of data {}".format(len(res.data)))

    for item in res.get("data"):
        print("-------")
        print(item)

    res = client.list_service(id='cattle-system/rancher')
    print("count of data {}".format(len(res.data)))

    for item in res.get("data"):
        print("-------")
        print(item)

