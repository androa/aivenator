// Code generated by mockery v2.7.4. DO NOT EDIT.

package mocks

import (
	aiven "github.com/aiven/aiven-go-client"
	mock "github.com/stretchr/testify/mock"
)

// ServiceUserManager is an autogenerated mock type for the ServiceUserManager type
type ServiceUserManager struct {
	mock.Mock
}

// Create provides a mock function with given fields: serviceUserName, projectName, serviceName
func (_m *ServiceUserManager) Create(serviceUserName string, projectName string, serviceName string) (*aiven.ServiceUser, error) {
	ret := _m.Called(serviceUserName, projectName, serviceName)

	var r0 *aiven.ServiceUser
	if rf, ok := ret.Get(0).(func(string, string, string) *aiven.ServiceUser); ok {
		r0 = rf(serviceUserName, projectName, serviceName)
	} else {
		if ret.Get(0) != nil {
			r0 = ret.Get(0).(*aiven.ServiceUser)
		}
	}

	var r1 error
	if rf, ok := ret.Get(1).(func(string, string, string) error); ok {
		r1 = rf(serviceUserName, projectName, serviceName)
	} else {
		r1 = ret.Error(1)
	}

	return r0, r1
}

// Delete provides a mock function with given fields: serviceUserName, projectName, serviceName
func (_m *ServiceUserManager) Delete(serviceUserName string, projectName string, serviceName string) error {
	ret := _m.Called(serviceUserName, projectName, serviceName)

	var r0 error
	if rf, ok := ret.Get(0).(func(string, string, string) error); ok {
		r0 = rf(serviceUserName, projectName, serviceName)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}
