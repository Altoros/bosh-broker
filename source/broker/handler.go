package broker

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/nu7hatch/gouuid"
	"github.com/pivotal-cf/brokerapi"

	"github.com/s-matyukevich/bosh-broker/source/bosh"
	"github.com/s-matyukevich/bosh-broker/source/config"
	"github.com/s-matyukevich/bosh-broker/source/tmpl"
)

func NewHandler(config *config.Config) (Handler, error) {
	h := Handler{}
	h.config = config
	h.templates = make(map[string]*Templates, 0)
	h.instances = make(map[string]*ServiceInstance, 0)
	var err error
	h.bosh, h.boshUUID, err = bosh.NewBoshProxy(config.BoshTarget, config.BoshUser, config.BoshPassword)
	if err != nil {
		return h, err
	}
	for key, p := range config.Plans {
		t := &Templates{}
		t.ManifestTmpl, err = prepareTemplate(p.ManifestTemplate)
		if err != nil {
			return h, err
		}
		t.BindTmpl, err = prepareTemplate(p.BindTemplate)
		if err != nil {
			return h, err
		}
		t.UnbindTmpl, err = prepareTemplate(p.UnbindTemplate)
		if err != nil {
			return h, err
		}
		t.StemcellTmpl, err = tmpl.NewTemplate(p.Stemcell)
		if err != nil {
			return h, err
		}
		t.ReleaseTmpl, err = tmpl.NewTemplate(p.Release)
		if err != nil {
			return h, err
		}
		h.templates[key] = t
	}
	return h, nil
}

func prepareTemplate(path string) (*tmpl.Template, error) {
	if path == "" {
		return nil, nil
	}
	str, err := ioutil.ReadFile(filepath.Join("templates", path))
	if err != nil {
		return nil, err
	}
	return tmpl.NewTemplate(string(str))
}

type Handler struct {
	config    *config.Config
	templates map[string]*Templates
	instances map[string]*ServiceInstance
	bosh      *bosh.BoshProxy
	boshUUID  string
}

func (h Handler) Services() []brokerapi.Service {
	service := brokerapi.Service{
		ID:            h.config.BrokerId,
		Name:          "bosh",
		Description:   "Bosh Service Broker",
		Bindable:      true,
		PlanUpdatable: false,
	}
	for key, p := range h.config.Plans {
		service.Plans = append(service.Plans, brokerapi.ServicePlan{
			ID:          key,
			Name:        p.Name,
			Description: p.Description,
		})
	}
	return []brokerapi.Service{service}
}

func (h Handler) Provision(instanceID string, details brokerapi.ProvisionDetails, _ bool) (brokerapi.ProvisionedServiceSpec, error) {
	s := brokerapi.ProvisionedServiceSpec{
		IsAsync:      true,
		DashboardURL: "",
	}
	config := h.config.Plans[details.PlanID]
	templates := h.templates[details.PlanID]
	params := make(map[string]interface{}, 0)
	if details.RawParameters != nil {
		err := json.Unmarshal(details.RawParameters, &params)
		if err != nil {
			return s, err
		}
	}
	service := &ServiceInstance{config, templates, params, ""}
	var err error
	service.LastTaskId, err = h.doDeployment(instanceID, service)
	if err != nil {
		return s, err
	}
	h.instances[instanceID] = service
	return s, err
}

func (h Handler) Deprovision(instanceID string, _ brokerapi.DeprovisionDetails, _ bool) (brokerapi.IsAsync, error) {
	deploymentPath := fmt.Sprintf("deployments/%s/", instanceID)
	err := os.RemoveAll(deploymentPath)
	if err != nil {
		return true, err
	}
	service := h.instances[instanceID]
	service.LastTaskId, err = h.bosh.DeleteDeployment("deployment" + instanceID)
	return true, err
}

func (h Handler) Bind(instanceID, bindingID string, details brokerapi.BindDetails) (brokerapi.Binding, error) {
	service := h.instances[instanceID]
	b := brokerapi.Binding{}
	bindPath := fmt.Sprintf("deployments/%s/%s_bind.sh", instanceID, bindingID)
	err := service.Templates.BindTmpl.ExecuteAndSave(service.InstanceParams, bindPath, 0777)
	if err != nil {
		return b, err
	}
	cmd := exec.Command(bindPath)
	out, err := cmd.Output()
	if err != nil {
		return b, err
	}
	b.Credentials = make(map[string]interface{}, 0)
	err = json.Unmarshal(out, &b.Credentials)
	return b, err
}

func (h Handler) Unbind(instanceID, bindingID string, _ brokerapi.UnbindDetails) error {
	service := h.instances[instanceID]
	if service.Templates.UnbindTmpl != nil {
		unbindPath := fmt.Sprintf("deployments/%s/%s_unbind.sh", instanceID, bindingID)
		err := service.Templates.UnbindTmpl.ExecuteAndSave(service.InstanceParams, unbindPath, 0777)
		if err != nil {
			return err
		}
		cmd := exec.Command(unbindPath)
		return cmd.Run()
	}
	return nil
}

func (h Handler) LastOperation(instanceID string) (brokerapi.LastOperation, error) {
	op := brokerapi.LastOperation{}
	service := h.instances[instanceID]
	status, err := h.bosh.Status(service.LastTaskId)
	if err != nil {
		return op, err
	}
	switch status {
	case "queued", "processing":
		op.State = brokerapi.InProgress
	case "done":
		op.State = brokerapi.Succeeded
	case "fail":
		op.State = brokerapi.Failed
	default:
		err = fmt.Errorf("unknown tasks status: %s", status)
	}

	return op, err
}

func (h Handler) Update(instanceID string, details brokerapi.UpdateDetails, _ bool) (brokerapi.IsAsync, error) {
	service := h.instances[instanceID]
	var err error
	service.LastTaskId, err = h.doDeployment(instanceID, service)
	return true, err
}

func (h Handler) doDeployment(instanceID string, s *ServiceInstance) (string, error) {
	err := h.prepareParams(instanceID, s.InstanceParams, s.Config)
	if err != nil {
		return "", err
	}
	deploymentPath := fmt.Sprintf("deployments/%s/manifest.yml", instanceID)
	err = s.Templates.ManifestTmpl.ExecuteAndSave(s.InstanceParams, deploymentPath, 0660)
	if err != nil {
		return "", err
	}
	release, err := s.Templates.ReleaseTmpl.Execute(s.InstanceParams)
	if err != nil {
		return "", err
	}
	stemcell, err := s.Templates.StemcellTmpl.Execute(s.InstanceParams)
	if err != nil {
		return "", err
	}
	err = h.bosh.UploadStemcell(stemcell)
	if err != nil {
		return "", err
	}
	err = h.bosh.UploadRelease(release)
	if err != nil {
		return "", err
	}
	return h.bosh.Deploy(deploymentPath)
}

func (h Handler) prepareParams(instanceID string, params map[string]interface{}, plan *config.ServicePlan) error {
	for _, p := range plan.Params {
		if _, ok := params[p.Name]; ok {
			continue
		}
		if p.Default != nil {
			params[p.Name] = p.Default
		} else if p.Random {
			u, err := uuid.NewV4()
			if err != nil {
				return err
			}
			params[p.Name] = u.String()
		} else if p.Optional {
			return fmt.Errorf("Required parameter %s is not set", p.Name)
		}
	}
	params["deployment_name"] = "deployment" + instanceID
	params["instance_id"] = instanceID
	params["director_uuid"] = h.boshUUID
	params["bosh_user"] = h.config.BoshUser
	params["bosh_password"] = h.config.BoshPassword
	return nil
}
