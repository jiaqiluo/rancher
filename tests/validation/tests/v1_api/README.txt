# Naming the testing methods

The format
    test_{resource_name}_[with_{field}]_{operation}[_{order_number}]
where:
- resource_name: [required] the name of the resource to test.
                            Examples: namespace, deployment, monitoring
- field:         [optional] the name of the addition field added to the basic.
                            Examples: secret, lb, label
- operation:     [required] the verb of operation.
                            Examples: create, update, delete
- order_number:  [optional] when there are more than 1 test case, the number starts from 2

Examples:
- test_deployment()
- test_deployment_update()
- test_deployment_update()
- test_deployment_update_2()
- test_deployment_with_secret_create()
- test_deployment_with_secret_update()
- test_monitoring_enable()
- test_monitoring_update()
- test_notifier_create()