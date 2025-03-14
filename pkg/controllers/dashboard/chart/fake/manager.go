// Code generated by MockGen. DO NOT EDIT.
// Source: chart.go
//
// Generated by this command:
//
//	mockgen -package=fake -destination=fake/manager.go -source=chart.go Manager
//

// Package fake is a generated GoMock package.
package fake

import (
	reflect "reflect"

	gomock "go.uber.org/mock/gomock"
)

// MockManager is a mock of Manager interface.
type MockManager struct {
	ctrl     *gomock.Controller
	recorder *MockManagerMockRecorder
}

// MockManagerMockRecorder is the mock recorder for MockManager.
type MockManagerMockRecorder struct {
	mock *MockManager
}

// NewMockManager creates a new mock instance.
func NewMockManager(ctrl *gomock.Controller) *MockManager {
	mock := &MockManager{ctrl: ctrl}
	mock.recorder = &MockManagerMockRecorder{mock}
	return mock
}

// EXPECT returns an object that allows the caller to indicate expected use.
func (m *MockManager) EXPECT() *MockManagerMockRecorder {
	return m.recorder
}

// Ensure mocks base method.
func (m *MockManager) Ensure(namespace, chartName, releaseName, minVersion, exactVersion string, values map[string]any, takeOwnership bool, installImageOverride string) error {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "Ensure", namespace, chartName, releaseName, minVersion, exactVersion, values, takeOwnership, installImageOverride)
	ret0, _ := ret[0].(error)
	return ret0
}

// Ensure indicates an expected call of Ensure.
func (mr *MockManagerMockRecorder) Ensure(namespace, chartName, releaseName, minVersion, exactVersion, values, takeOwnership, installImageOverride any) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "Ensure", reflect.TypeOf((*MockManager)(nil).Ensure), namespace, chartName, releaseName, minVersion, exactVersion, values, takeOwnership, installImageOverride)
}

// Remove mocks base method.
func (m *MockManager) Remove(namespace, releaseName string) {
	m.ctrl.T.Helper()
	m.ctrl.Call(m, "Remove", namespace, releaseName)
}

// Remove indicates an expected call of Remove.
func (mr *MockManagerMockRecorder) Remove(namespace, releaseName any) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "Remove", reflect.TypeOf((*MockManager)(nil).Remove), namespace, releaseName)
}

// Uninstall mocks base method.
func (m *MockManager) Uninstall(namespace, releaseName string) error {
	m.ctrl.T.Helper()
	ret := m.ctrl.Call(m, "Uninstall", namespace, releaseName)
	ret0, _ := ret[0].(error)
	return ret0
}

// Uninstall indicates an expected call of Uninstall.
func (mr *MockManagerMockRecorder) Uninstall(namespace, releaseName any) *gomock.Call {
	mr.mock.ctrl.T.Helper()
	return mr.mock.ctrl.RecordCallWithMethodType(mr.mock, "Uninstall", reflect.TypeOf((*MockManager)(nil).Uninstall), namespace, releaseName)
}
