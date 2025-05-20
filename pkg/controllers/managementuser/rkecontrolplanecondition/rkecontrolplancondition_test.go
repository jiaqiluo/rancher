package rkecontrolplanecondition

import (
	"fmt"
	"testing"
	"time"

	catalog "github.com/rancher/rancher/pkg/apis/catalog.cattle.io/v1"
	v1 "github.com/rancher/rancher/pkg/apis/rke.cattle.io/v1"
	"github.com/rancher/rancher/pkg/capr"
	"github.com/rancher/rancher/pkg/namespace"
	"github.com/rancher/rancher/pkg/settings"
	"github.com/rancher/wrangler/v3/pkg/generic/fake"
	"go.uber.org/mock/gomock"
	apierror "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
	mgmtClusterName  = "c-mx21351"
	provClusterName  = "dev-cluster"
	controlPlaneName = "cp-58542"
)

func Test_handler_syncSystemUpgradeControllerStatus(t *testing.T) {
	type config struct {
		// The mgmt cluster that the mock handler is registered for
		mgmtClusterName string
		// The App that the mock handler returns
		app *catalog.App
		// The error that the mock handler returns
		err error
		// The value for set the SystemUpgradeControllerChartVersion setting
		chartVersion string
	}

	tests := []struct {
		name  string
		setup config
		input *v1.RKEControlPlane

		wantError                bool
		wantedSUCConditionStatus string
		appClientIsInvoked       bool
	}{
		{
			name: "rkeControlPlane is being deleted",
			input: &v1.RKEControlPlane{
				ObjectMeta: metav1.ObjectMeta{
					Name:              controlPlaneName,
					Namespace:         namespace.System,
					DeletionTimestamp: &metav1.Time{Time: time.Now()},
				},
				Spec: v1.RKEControlPlaneSpec{
					ClusterName: provClusterName,
				},
				Status: v1.RKEControlPlaneStatus{},
			},
			wantError:                false,
			wantedSUCConditionStatus: "",
			appClientIsInvoked:       false,
		},
		{
			name: "rkeControlPlane is for a different cluster",
			setup: config{
				mgmtClusterName: mgmtClusterName,
			},
			input: &v1.RKEControlPlane{
				ObjectMeta: metav1.ObjectMeta{
					Name:      controlPlaneName,
					Namespace: namespace.System,
				},
				Spec: v1.RKEControlPlaneSpec{
					ManagementClusterName: "another-cluster",
				},
				Status: v1.RKEControlPlaneStatus{},
			},
			wantError:                false,
			wantedSUCConditionStatus: "",
			appClientIsInvoked:       false,
		},
		{
			name: "fail to get the app with notFound error",
			setup: config{
				mgmtClusterName: mgmtClusterName,
				app: &catalog.App{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "another-app",
						Namespace: namespace.System,
					},
					Spec:   catalog.ReleaseSpec{},
					Status: catalog.ReleaseStatus{},
				},
				err: apierror.NewNotFound(catalog.Resource("app"), appName(provClusterName)),
			},
			input: &v1.RKEControlPlane{
				ObjectMeta: metav1.ObjectMeta{
					Name:      controlPlaneName,
					Namespace: namespace.System,
				},
				Spec: v1.RKEControlPlaneSpec{
					ManagementClusterName: mgmtClusterName,
					ClusterName:           provClusterName,
				},
				Status: v1.RKEControlPlaneStatus{},
			},
			wantError:                false,
			wantedSUCConditionStatus: "False",
			appClientIsInvoked:       true,
		},
		{
			name: "fail to get the app with non-notFound error",
			setup: config{
				mgmtClusterName: mgmtClusterName,
				app: &catalog.App{
					ObjectMeta: metav1.ObjectMeta{
						Name:      appName(provClusterName),
						Namespace: namespace.System,
					},
					Spec:   catalog.ReleaseSpec{},
					Status: catalog.ReleaseStatus{},
				},
				err: apierror.NewInternalError(fmt.Errorf("something goes wrong")),
			},
			input: &v1.RKEControlPlane{
				ObjectMeta: metav1.ObjectMeta{
					Name:      controlPlaneName,
					Namespace: namespace.System,
				},
				Spec: v1.RKEControlPlaneSpec{
					ManagementClusterName: mgmtClusterName,
					ClusterName:           provClusterName,
				},
				Status: v1.RKEControlPlaneStatus{},
			},
			wantError:                true,
			wantedSUCConditionStatus: "",
			appClientIsInvoked:       true,
		},
		{
			name: "app is being deleted",
			setup: config{
				mgmtClusterName: mgmtClusterName,
				app: &catalog.App{
					ObjectMeta: metav1.ObjectMeta{
						Name:              appName(provClusterName),
						Namespace:         namespace.System,
						DeletionTimestamp: &metav1.Time{Time: time.Now()},
					},
					Spec:   catalog.ReleaseSpec{},
					Status: catalog.ReleaseStatus{},
				},
				err: nil,
			},
			input: &v1.RKEControlPlane{
				ObjectMeta: metav1.ObjectMeta{
					Name:      controlPlaneName,
					Namespace: namespace.System,
				},
				Spec: v1.RKEControlPlaneSpec{
					ClusterName:           provClusterName,
					ManagementClusterName: mgmtClusterName,
				},
				Status: v1.RKEControlPlaneStatus{},
			},
			wantError:                false,
			wantedSUCConditionStatus: "False",
			appClientIsInvoked:       true,
		},
		{
			name: "app's chart version is out of sync",
			setup: config{
				mgmtClusterName: mgmtClusterName,
				app: &catalog.App{
					ObjectMeta: metav1.ObjectMeta{
						Name:      appName(provClusterName),
						Namespace: namespace.System,
					},
					Spec: catalog.ReleaseSpec{
						Chart: &catalog.Chart{
							Metadata: &catalog.Metadata{
								Version: "160.0.0",
							},
						},
					},
					Status: catalog.ReleaseStatus{},
				},
				err:          nil,
				chartVersion: "160.1.0",
			},
			input: &v1.RKEControlPlane{
				ObjectMeta: metav1.ObjectMeta{
					Name:      controlPlaneName,
					Namespace: namespace.System,
				},
				Spec: v1.RKEControlPlaneSpec{
					ClusterName:           provClusterName,
					ManagementClusterName: mgmtClusterName,
				},
				Status: v1.RKEControlPlaneStatus{},
			},
			wantError:                false,
			wantedSUCConditionStatus: "False",
			appClientIsInvoked:       true,
		},
		{
			name: "app is deployed",
			setup: config{
				mgmtClusterName: mgmtClusterName,
				app: &catalog.App{
					ObjectMeta: metav1.ObjectMeta{
						Name:      appName(provClusterName),
						Namespace: namespace.System,
					},
					Spec: catalog.ReleaseSpec{
						Chart: &catalog.Chart{
							Metadata: &catalog.Metadata{
								Version: "160.1.0",
							},
						},
					},
					Status: catalog.ReleaseStatus{
						Summary: catalog.Summary{
							State:         string(catalog.StatusDeployed),
							Error:         false,
							Transitioning: false,
						},
					},
				},
				err:          nil,
				chartVersion: "160.1.0",
			},
			input: &v1.RKEControlPlane{
				ObjectMeta: metav1.ObjectMeta{
					Name:      controlPlaneName,
					Namespace: namespace.System,
				},
				Spec: v1.RKEControlPlaneSpec{
					ClusterName:           provClusterName,
					ManagementClusterName: mgmtClusterName,
				},
				Status: v1.RKEControlPlaneStatus{},
			},
			wantError:                false,
			wantedSUCConditionStatus: "True",
			appClientIsInvoked:       true,
		},
		{
			name: "app is in error wantedSUCConditionStatus",
			setup: config{
				mgmtClusterName: mgmtClusterName,
				app: &catalog.App{
					ObjectMeta: metav1.ObjectMeta{
						Name:      appName(provClusterName),
						Namespace: namespace.System,
					},
					Spec: catalog.ReleaseSpec{
						Chart: &catalog.Chart{
							Metadata: &catalog.Metadata{
								Version: "160.1.0",
							},
						},
					},
					Status: catalog.ReleaseStatus{
						Summary: catalog.Summary{
							State:         string(catalog.StatusFailed),
							Error:         true,
							Transitioning: false,
						},
					},
				},
				err:          nil,
				chartVersion: "160.1.0",
			},
			input: &v1.RKEControlPlane{
				ObjectMeta: metav1.ObjectMeta{
					Name:      controlPlaneName,
					Namespace: namespace.System,
				},
				Spec: v1.RKEControlPlaneSpec{
					ClusterName:           provClusterName,
					ManagementClusterName: mgmtClusterName,
				},
				Status: v1.RKEControlPlaneStatus{},
			},
			wantError:                false,
			wantedSUCConditionStatus: "False",
			appClientIsInvoked:       true,
		},
		{
			name: "app is transitioning",
			setup: config{
				mgmtClusterName: mgmtClusterName,
				app: &catalog.App{
					ObjectMeta: metav1.ObjectMeta{
						Name:      appName(provClusterName),
						Namespace: namespace.System,
					},
					Spec: catalog.ReleaseSpec{
						Chart: &catalog.Chart{
							Metadata: &catalog.Metadata{
								Version: "160.1.0",
							},
						},
					},
					Status: catalog.ReleaseStatus{
						Summary: catalog.Summary{
							State:         string(catalog.StatusPendingInstall),
							Error:         false,
							Transitioning: true,
						},
					},
				},
				err:          nil,
				chartVersion: "160.1.0",
			},
			input: &v1.RKEControlPlane{
				ObjectMeta: metav1.ObjectMeta{
					Name:      controlPlaneName,
					Namespace: namespace.System,
				},
				Spec: v1.RKEControlPlaneSpec{
					ClusterName:           provClusterName,
					ManagementClusterName: mgmtClusterName,
				},
				Status: v1.RKEControlPlaneStatus{},
			},
			wantError:                false,
			wantedSUCConditionStatus: "Unknown",
			appClientIsInvoked:       true,
		},
		{
			name: "app is uninstalled",
			setup: config{
				mgmtClusterName: mgmtClusterName,
				app: &catalog.App{
					ObjectMeta: metav1.ObjectMeta{
						Name:      appName(provClusterName),
						Namespace: namespace.System,
					},
					Spec: catalog.ReleaseSpec{
						Chart: &catalog.Chart{
							Metadata: &catalog.Metadata{
								Version: "160.1.0",
							},
						},
					},
					Status: catalog.ReleaseStatus{
						Summary: catalog.Summary{
							State:         string(catalog.StatusUninstalled),
							Error:         false,
							Transitioning: false,
						},
					},
				},
				err:          nil,
				chartVersion: "160.1.0",
			},
			input: &v1.RKEControlPlane{
				ObjectMeta: metav1.ObjectMeta{
					Name:      controlPlaneName,
					Namespace: namespace.System,
				},
				Spec: v1.RKEControlPlaneSpec{
					ClusterName:           provClusterName,
					ManagementClusterName: mgmtClusterName,
				},
				Status: v1.RKEControlPlaneStatus{},
			},
			wantError:                false,
			wantedSUCConditionStatus: "Unknown",
			appClientIsInvoked:       true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			bc := fake.NewMockControllerInterface[*catalog.App, *catalog.AppList](ctrl)
			h := &handler{
				mgmtClusterName:         tt.setup.mgmtClusterName,
				downstreamAppController: bc,
			}
			if tt.appClientIsInvoked {
				bc.EXPECT().Get(namespace.System, appName(tt.input.Spec.ClusterName), metav1.GetOptions{}).Return(tt.setup.app, tt.setup.err)
			}

			if tt.setup.chartVersion != "" {
				current := settings.SystemUpgradeControllerChartVersion.Get()
				if err := settings.SystemUpgradeControllerChartVersion.Set(tt.setup.chartVersion); err != nil {
					t.Errorf("failed to set up : %v", err)
				}
				defer func() {
					err := settings.SystemUpgradeControllerChartVersion.Set(current)
					if err != nil {

					}
				}()
			}
			got, err := h.syncSystemUpgradeControllerStatus(tt.input, tt.input.Status)

			if (err != nil) != tt.wantError {
				t.Errorf("syncSystemUpgradeControllerStatus() error = %v, wantError %v", err, tt.wantError)
				return
			}
			// Check the condition's status value instead of the entire object,
			// as it includes a lastUpdateTime field that is difficult to mock
			if capr.SystemUpgradeControllerReady.GetStatus(&got) != tt.wantedSUCConditionStatus {
				t.Errorf("syncSystemUpgradeControllerStatus() got = %v, expected SystemUpgradeControllerReady condition status value = %v", got, tt.wantedSUCConditionStatus)
			}
		})
	}
}
