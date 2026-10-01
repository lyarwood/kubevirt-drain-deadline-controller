package controller_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestControllerUnit(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "VMI Eviction Controller Unit Suite")
}
