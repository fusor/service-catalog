/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"strings"

	"github.com/golang/glog"
	"github.com/kubernetes-incubator/service-catalog/contrib/pkg/broker/controller"
	"github.com/kubernetes-incubator/service-catalog/pkg/brokerapi"
	"github.com/kubernetes-incubator/service-catalog/pkg/util"
)

type resourceType int

const (
	serviceInstance = iota
	serviceBinding
)

const (
	catalogFmt        = "http://%s:%d/services"
	getServiceByIDFmt = "http://%s:%d/services/%s"

	instanceNameFmt = "cf-i-%s"
	bindingNameFmt  = "cf-b-%s"
)

type binding struct {
}

type instance struct {
}

type k8sController struct {
	instances map[string]*instance
	bindings  map[string]*binding

	registryHost string
	registryPort int

	reifier Reifier
}

// CreateController creates an instance of a Kubernetes broker controller.
func CreateController(host string, port int, reifier Reifier) (controller.Controller, error) {
	return &k8sController{
		instances:    make(map[string]*instance),
		bindings:     make(map[string]*binding),
		registryHost: host,
		registryPort: port,
		reifier:      reifier,
	}, nil
}

func (c *k8sController) Catalog() (*brokerapi.Catalog, error) {
	u := fmt.Sprintf(catalogFmt, c.registryHost, c.registryPort)

	var services []*brokerapi.Service
	err := util.FetchObject(u, &services)
	if err != nil {
		glog.Errorf("Failed to fetch catalog from service registry: %v\n", err)
		return nil, err
	}

	// For each service plan, we need to fetch the corresponding instance/binding type schemas
	// and stuff them in with the service response.
	for i, s := range services {
		for j, sp := range s.Plans {
			types, err := getTypesFromPlan(&sp)
			if err != nil {
				glog.Errorf("Failed to fetch schemas for types: %v\n", err)
				return nil, err
			}

			schemas, err := c.getSchemas(*types)
			if err != nil {
				glog.Errorf("Failed to fetch schemas for types: %v\n", err)
				return nil, err
			}

			services[i].Plans[j].Schemas = schemas
		}
	}
	return &brokerapi.Catalog{Services: services}, nil
}

func (c *k8sController) CreateServiceInstance(instanceID string, req *brokerapi.ServiceInstanceRequest) (*brokerapi.CreateServiceInstanceResponse, error) {
	// Fetch the type that should be used for this service/plan
	t, err := c.getType(req.ServiceID, req.PlanID)
	if err != nil {
		glog.Errorf("Can't find a type for %s:%s : %v", req.ServiceID, req.PlanID, err)
		return nil, err

	}
	// Create a temp file that we'll use to pull this chart into.
	f, err := ioutil.TempFile("", "chart-")
	if err != nil {
		glog.Errorf("Failed to create TempFile for chart download: %v", err)
		return nil, err
	}
	defer os.Remove(f.Name())
	err = util.FetchChartToFile(t.Instance, f)
	if err != nil {
		glog.Errorf("Failed to fetch %s : %v", t.Instance, err)
		return nil, err
	}

	instanceName := createResourceName(instanceID, serviceInstance)

	ret, err := c.reifier.CreateServiceInstance(instanceName, f.Name(), req)
	if err != nil {
		glog.Errorf("Failed to create service instance %s : %v", t.Instance, err)
		return nil, err
	}

	glog.Infof("Created service instance:\n%v\n", ret)
	return ret, nil
}

func (c *k8sController) GetServiceInstance(instanceID string) (string, error) {
	return "", errors.New("Unimplemented")
}

func (c *k8sController) RemoveServiceInstance(instanceID string) error {
	instanceName := createResourceName(instanceID, serviceInstance)

	err := c.reifier.RemoveServiceInstance(instanceName)
	if err != nil {
		glog.Errorf("Failed to remove %s : %v", instanceID, err)
		return err
	}

	return nil
}

func (c *k8sController) Bind(instanceID string, bindingID string, req *brokerapi.BindingRequest) (*brokerapi.CreateServiceBindingResponse, error) {
	// Bind to the instance.
	instanceName := createResourceName(instanceID, serviceInstance)

	// First we need to create the actual binding. This will result in the binding
	// in the actual Service Instance.
	siBinding, err := c.reifier.CreateServiceBinding(instanceName, req)
	if err != nil {
		glog.Errorf("Failed to create service binding %s : %v", instanceID, err)
		return nil, err
	}
	glog.Infof("Got binding as\n%s\n", siBinding)

	return siBinding, nil
}

func (c *k8sController) UnBind(instanceID string, bindingID string) error {
	instanceName := createResourceName(instanceID, serviceInstance)
	bindingName := createResourceName(bindingID, serviceBinding)
	err := c.reifier.RemoveServiceBinding(instanceName)
	if err != nil {
		glog.Errorf("Failed to remove binding %s : %v", instanceID, err)
		return err
	}

	err = c.reifier.RemoveServiceInstance(bindingName)
	if err != nil {
		glog.Errorf("Cannot remove proxy %s for binding %s\n", bindingName, bindingID)
	}

	return nil
}

func (c *k8sController) getType(serviceID string, planID string) (*brokerapi.Types, error) {
	s, err := c.getServiceByID(serviceID)
	if err != nil {
		return nil, err
	}

	for _, p := range s.Plans {
		if p.ID == planID {
			return getTypesFromPlan(&p)
		}
	}
	return nil, fmt.Errorf("Did not find plan: %s", planID)
}

func getTypesFromPlan(p *brokerapi.ServicePlan) (*brokerapi.Types, error) {
	err := fmt.Errorf("Did not find usable types for plan %s", p.ID)

	if p.Metadata == nil {
		return nil, err
	}
	if _, ok := p.Metadata.(map[string]interface{})[brokerapi.InstanceType]; !ok {
		return nil, err
	}
	if _, ok := p.Metadata.(map[string]interface{})[brokerapi.BindingType]; !ok {
		// No binding type... cool, just return the instance type
		return &brokerapi.Types{
			Instance: p.Metadata.(map[string]interface{})[brokerapi.InstanceType].(string),
		}, nil
	}
	return &brokerapi.Types{
		Instance: p.Metadata.(map[string]interface{})[brokerapi.InstanceType].(string),
		Binding:  p.Metadata.(map[string]interface{})[brokerapi.BindingType].(string),
	}, nil
}

func (c *k8sController) getSchemas(t brokerapi.Types) (*brokerapi.Schemas, error) {
	is, err := getSchema(t.Instance)
	if err != nil {
		return nil, err
	}

	s := &brokerapi.Schemas{Instance: *is}

	// May not be bindable, and thus won't have a binding type.
	if t.Binding != "" {
		bs, err := getSchema(t.Binding)
		if err != nil {
			return nil, err
		}

		s.Binding = *bs
	}
	return s, nil
}

func getSchema(t string) (*brokerapi.Schema, error) {
	if strings.HasPrefix(t, "gs://") {
		u := strings.Replace(t, "gs://", "https://storage.googleapis.com/", 1)
		u = u + ".schema"
		schema, err := util.Fetch(u)
		if err != nil {
			return nil, err
		}
		return &brokerapi.Schema{Inputs: string(schema)}, nil
	}
	return nil, errors.New("invalid url format; gs://... is required")
}

func (c *k8sController) getServiceByID(id string) (*brokerapi.Service, error) {
	u := fmt.Sprintf(getServiceByIDFmt, c.registryHost, c.registryPort, id)
	var service brokerapi.Service
	err := util.FetchObject(u, &service)
	return &service, err
}

// createResourceName converts a UUID to a resource name.
func createResourceName(id string, t resourceType) string {
	cleanName := strings.Replace(id, "-", "", -1)
	switch t {
	case serviceInstance:
		return fmt.Sprintf(instanceNameFmt, cleanName)
	case serviceBinding:
		return fmt.Sprintf(bindingNameFmt, cleanName)
	}
	return ""
}
