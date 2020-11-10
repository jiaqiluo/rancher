# how to write RBAC-related testing functions
A set of global variables and functions are introduced to make it easy
to write tests for rancher's features around RBAC.

When the testing environment is initializing, the following resources are created:
- a new project that new users and user roles are bound to
- a namespace in that project
- five standard users
- another unshared project that no of those users are bound to
- a namespace and a workload in this project

Those users are assigned to different roles:
 - cluster owner
 - cluster member
 - project owner
 - project member
 - project read-only member

The following functions are provided:
- to get a specific user: `rbac_get_user_by_role(role_template_id)`
- to get a specific user's token: `rbac_get_user_token_by_role(role_template_id)`
- to get the project: `rbac_get_project()`
- to get the namespace in the project: `rbac_get_namespace()`
- to get the unshared proejct: `rbac_get_unshared_project()`
- to get the unshared namespace: `rbac_get_unshared_ns()`
- to get the unshared workload: `rbac_get_unshared_workload()`


The following are some functions using the above resources in `test_workload.py`:
- test_wl_rbac_cluster_owner
- test_wl_rbac_cluster_member
- test_wl_rbac_project_member


# how to use the new fixture: `remove_resource`
This fixture handles the deletion of any resource that is created in the
function. Here is an example:
```
def test_wl_rbac_project_member(remove_resource):
    ...
    workload = p_client.create_workload(name=name, containers=con, namespaceId=ns.id)
    remove_resource(workload)
    ...
```
First, we need to pass the fixture as an argument to the function to registry it to the scope of the function;
second, we need to call this fixture after creating a new resource, in this example it is a workload.
In such way, the workload will be removed automatically when this test finishes no matter it succeeds or fails.

# how to user the new rancher client for the new api v1
Before we dive into the technical details, let's clarify a confusion here: 

Yes, the new api is v1, and the old api is v3. 

The new api is v1 because it is the beginning of Rancher's new api framework,
and it is used for Rancher's new UI, the cluster explorer, to talk with the backend. 


The support for the new v1 api is added to the Rancher client module. 
Currenly there are 2 levels of the client: global client and cluster clint. 
(The old v3 api has one more: project client)


