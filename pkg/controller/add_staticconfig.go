package controller

import (
	"github.com/openshift/static-config-operator/pkg/controller/staticconfig"
)

func init() {
	// AddToManagerFuncs is a list of functions to create controllers and add them to a manager.
	AddToManagerFuncs = append(AddToManagerFuncs, staticconfig.Add)
}
