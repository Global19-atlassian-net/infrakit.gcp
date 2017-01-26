package group

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"

	log "github.com/Sirupsen/logrus"
	"github.com/docker/infrakit.gcp/plugin/gcloud"
	instance_types "github.com/docker/infrakit.gcp/plugin/instance/types"
	group_plugin "github.com/docker/infrakit/pkg/plugin/group"
	"github.com/docker/infrakit/pkg/plugin/group/types"
	"github.com/docker/infrakit/pkg/spi/group"
	"github.com/docker/infrakit/pkg/spi/instance"
)

type settings struct {
	spec               types.Spec
	groupSpec          group.Spec
	instanceSpec       instance.Spec
	instanceProperties instance_types.Properties
	currentTemplate    int
	createdTemplates   []string
}

type plugin struct {
	API           gcloud.API
	flavorPlugins group_plugin.FlavorPluginLookup
	groups        map[group.ID]settings
	lock          sync.Mutex
}

// NewGCEGroupPlugin creates a new GCE group plugin for a given project
// and zone.
func NewGCEGroupPlugin(project, zone string, flavorPlugins group_plugin.FlavorPluginLookup) group.Plugin {
	api, err := gcloud.New(project, zone)
	if err != nil {
		log.Fatal(err)
	}

	return &plugin{
		API:           api,
		flavorPlugins: flavorPlugins,
		groups:        map[group.ID]settings{},
	}
}

func (p *plugin) validate(groupSpec group.Spec) (settings, error) {
	noSettings := settings{}

	if groupSpec.ID == "" {
		return noSettings, errors.New("Group ID must not be blank")
	}

	spec, err := types.ParseProperties(groupSpec)
	if err != nil {
		return noSettings, err
	}

	if spec.Allocation.LogicalIDs != nil {
		return noSettings, errors.New("Allocation.LogicalIDs is not supported")
	}

	if spec.Allocation.Size <= 0 {
		return noSettings, errors.New("Allocation must be > 0")
	}

	flavorPlugin, err := p.flavorPlugins(spec.Flavor.Plugin)
	if err != nil {
		return noSettings, fmt.Errorf("Failed to find Flavor plugin '%s':%v", spec.Flavor.Plugin, err)
	}

	err = flavorPlugin.Validate(types.RawMessage(spec.Flavor.Properties), spec.Allocation)
	if err != nil {
		return noSettings, err
	}

	rawInstanceProperties := json.RawMessage(spec.Instance.Properties.Bytes())

	instanceSpec := instance.Spec{
		Tags:       map[string]string{},
		Properties: &rawInstanceProperties,
	}

	instanceSpec, err = flavorPlugin.Prepare(types.RawMessage(spec.Flavor.Properties), instanceSpec, spec.Allocation)
	if err != nil {
		return noSettings, err
	}

	instanceProperties, err := instance_types.ParseProperties(instance_types.RawMessage(instanceSpec.Properties))
	if err != nil {
		return noSettings, err
	}

	return settings{
		spec:               spec,
		groupSpec:          groupSpec,
		instanceSpec:       instanceSpec,
		instanceProperties: instanceProperties,
		currentTemplate:    1,
	}, nil
}

// TODO: handle reusing existing group
func (p *plugin) CommitGroup(config group.Spec, pretend bool) (string, error) {
	p.lock.Lock()
	defer p.lock.Unlock()

	newSettings, err := p.validate(config)
	if err != nil {
		return "", err
	}

	log.Infof("Committing group %s (pretend=%t)", config.ID, pretend)

	name := string(config.ID)
	targetSize := int64(newSettings.spec.Allocation.Size)

	operations := []string{}
	createManager := false
	createTemplate := false
	updateManager := false
	resize := false

	settings, present := p.groups[config.ID]
	if !present {
		settings = newSettings

		operations = append(operations, fmt.Sprintf("Managing %d instances", targetSize))
		createManager = true
		createTemplate = true
	} else {
		if !reflect.DeepEqual(settings.instanceProperties, newSettings.instanceProperties) {
			operations = append(operations, "Updating instance template")
			createTemplate = true
			if !pretend {
				settings.currentTemplate++
			}
		}

		if settings.spec.Allocation.Size != newSettings.spec.Allocation.Size {
			operations = append(operations, fmt.Sprintf("Scaling group to %d instance.", targetSize))
			resize = true
		}
	}

	if !pretend {
		templateName := fmt.Sprintf("%s-%d", name, settings.currentTemplate)
		settings.createdTemplates = append(settings.createdTemplates, templateName)

		if createTemplate {
			metadata, err := instance_types.ParseMetadata(settings.instanceSpec)
			if err != nil {
				return "", err
			}

			if err = p.API.CreateInstanceTemplate(templateName, &gcloud.InstanceSettings{
				Description:       settings.instanceProperties.Description,
				MachineType:       settings.instanceProperties.MachineType,
				Network:           settings.instanceProperties.Network,
				Tags:              settings.instanceProperties.Tags,
				DiskSizeMb:        settings.instanceProperties.DiskSizeMb,
				DiskImage:         settings.instanceProperties.DiskImage,
				DiskType:          settings.instanceProperties.DiskType,
				Scopes:            settings.instanceProperties.Scopes,
				Preemptible:       settings.instanceProperties.Preemptible,
				AutoDeleteDisk:    true,
				ReuseExistingDisk: false,
				MetaData:          gcloud.TagsToMetaData(metadata),
			}); err != nil {
				return "", err
			}
		}

		if createManager {
			if err = p.API.CreateInstanceGroupManager(name, &gcloud.InstanceManagerSettings{
				TemplateName:     fmt.Sprintf("%s-%d", name, settings.currentTemplate),
				TargetSize:       targetSize,
				Description:      settings.instanceProperties.Description,
				TargetPool:       settings.instanceProperties.TargetPool,
				BaseInstanceName: settings.instanceProperties.NamePrefix,
			}); err != nil {
				return "", err
			}
		}

		if updateManager {
			// TODO: should be trigger a recreation of the VMS
			// TODO: What about the instances already being updated
			if err = p.API.SetInstanceTemplate(name, templateName); err != nil {
				return "", err
			}
		}

		if resize {
			err := p.API.ResizeInstanceGroupManager(name, targetSize)
			if err != nil {
				return "", err
			}
		}
	}

	p.groups[config.ID] = settings

	return strings.Join(operations, "\n"), nil
}

func (p *plugin) FreeGroup(id group.ID) error {
	p.lock.Lock()
	defer p.lock.Unlock()

	_, present := p.groups[id]
	if !present {
		return fmt.Errorf("This group is not being watched: '%s", id)
	}

	delete(p.groups, id)

	return nil
}

func (p *plugin) DescribeGroup(id group.ID) (group.Description, error) {
	noDescription := group.Description{}

	p.lock.Lock()
	defer p.lock.Unlock()

	currentSettings, present := p.groups[id]
	if !present {
		return noDescription, fmt.Errorf("This group is not being watched: '%s", id)
	}

	name := string(id)

	instanceGroupInstances, err := p.API.ListInstanceGroupInstances(name)
	if err != nil {
		return noDescription, err
	}

	instances := []instance.Description{}

	for _, grpInst := range instanceGroupInstances {
		name := last(grpInst.Instance)

		inst, err := p.API.GetInstance(name)
		if err != nil {
			return noDescription, err
		}

		instances = append(instances, instance.Description{
			ID:   instance.ID(inst.Name),
			Tags: gcloud.MetaDataToTags(inst.Metadata.Items),
		})
	}

	return group.Description{
		Converged: len(instanceGroupInstances) == int(currentSettings.spec.Allocation.Size),
		Instances: instances,
	}, nil
}

func (p *plugin) DestroyGroup(id group.ID) error {
	p.lock.Lock()
	defer p.lock.Unlock()

	currentSettings, present := p.groups[id]
	if !present {
		return fmt.Errorf("This group is not being watched: '%s", id)
	}

	name := string(id)

	if err := p.API.DeleteInstanceGroupManager(name); err != nil {
		return err
	}

	for _, createdTemplate := range currentSettings.createdTemplates {
		if err := p.API.DeleteInstanceTemplate(createdTemplate); err != nil {
			return err
		}
	}

	delete(p.groups, id)

	return nil
}

func (p *plugin) InspectGroups() ([]group.Spec, error) {
	p.lock.Lock()
	defer p.lock.Unlock()

	specs := []group.Spec{}
	for _, spec := range p.groups {
		specs = append(specs, spec.groupSpec)
	}

	return specs, nil
}

func last(url string) string {
	parts := strings.Split(url, "/")
	return parts[len(parts)-1]
}