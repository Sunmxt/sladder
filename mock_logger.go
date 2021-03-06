// Code generated by mockery v1.0.0. DO NOT EDIT.

package sladder

import mock "github.com/stretchr/testify/mock"

// MockLogger is an autogenerated mock type for the Logger type
type MockLogger struct {
	mock.Mock
}

// Error provides a mock function with given fields: v
func (_m *MockLogger) Error(v ...interface{}) {
	var _ca []interface{}
	_ca = append(_ca, v...)
	_m.Called(_ca...)
}

// Errorf provides a mock function with given fields: format, v
func (_m *MockLogger) Errorf(format string, v ...interface{}) {
	var _ca []interface{}
	_ca = append(_ca, format)
	_ca = append(_ca, v...)
	_m.Called(_ca...)
}

// Fatal provides a mock function with given fields: v
func (_m *MockLogger) Fatal(v ...interface{}) {
	var _ca []interface{}
	_ca = append(_ca, v...)
	_m.Called(_ca...)
}

// Fatalf provides a mock function with given fields: format, v
func (_m *MockLogger) Fatalf(format string, v ...interface{}) {
	var _ca []interface{}
	_ca = append(_ca, format)
	_ca = append(_ca, v...)
	_m.Called(_ca...)
}

// Panic provides a mock function with given fields: v
func (_m *MockLogger) Panic(v ...interface{}) {
	var _ca []interface{}
	_ca = append(_ca, v...)
	_m.Called(_ca...)
}

// Panicf provides a mock function with given fields: format, v
func (_m *MockLogger) Panicf(format string, v ...interface{}) {
	var _ca []interface{}
	_ca = append(_ca, format)
	_ca = append(_ca, v...)
	_m.Called(_ca...)
}

// Print provides a mock function with given fields: v
func (_m *MockLogger) Print(v ...interface{}) {
	var _ca []interface{}
	_ca = append(_ca, v...)
	_m.Called(_ca...)
}

// Printf provides a mock function with given fields: format, v
func (_m *MockLogger) Printf(format string, v ...interface{}) {
	var _ca []interface{}
	_ca = append(_ca, format)
	_ca = append(_ca, v...)
	_m.Called(_ca...)
}

// Warn provides a mock function with given fields: v
func (_m *MockLogger) Warn(v ...interface{}) {
	var _ca []interface{}
	_ca = append(_ca, v...)
	_m.Called(_ca...)
}

// Warnf provides a mock function with given fields: format, v
func (_m *MockLogger) Warnf(format string, v ...interface{}) {
	var _ca []interface{}
	_ca = append(_ca, format)
	_ca = append(_ca, v...)
	_m.Called(_ca...)
}
