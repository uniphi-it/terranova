/*
Copyright The Terranova Authors

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

package terranova

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"sync"

	"github.com/hashicorp/terraform/internal/configs/configschema"
	"github.com/hashicorp/terraform/internal/configs/hcl2shim"
	"github.com/hashicorp/terraform/internal/plans/objchange"
	"github.com/hashicorp/terraform/internal/providers"
	"github.com/hashicorp/terraform/internal/tfdiags"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
)

const newExtraKey = "_new_extra_shim"

// Provider implements the provider.Interface wrapping the legacy
// ResourceProvider but not using gRPC like terraform does
type Provider struct {
	provider *schema.Provider
	mu       sync.Mutex
	schemas  providers.GetSchemaResponse
}

// NewProvider creates a Terranova Provider to wrap the given legacy ResourceProvider
func NewProvider(provider terraform.ResourceProvider) *Provider {
	sp, ok := provider.(*schema.Provider)
	if !ok {
		return nil
	}

	return &Provider{
		provider: sp,
	}
}

func providersFactory(rp terraform.ResourceProvider) providers.Factory {
	p := NewProvider(rp)
	return providers.FactoryFixed(p)
}

// GetSchema implements the GetSchema from providers.Interface. Returns the
// complete schema for the provider.
func (p *Provider) GetSchema() (resp providers.GetSchemaResponse) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.schemas.Provider.Block != nil {
		return p.schemas
	}

	resp = providers.GetSchemaResponse{
		ResourceTypes: make(map[string]providers.Schema),
		DataSources:   make(map[string]providers.Schema),
	}

	resp.Provider = providers.Schema{
		// Version: p.provider.Schema.Version,
		Block: schema.InternalMap(p.provider.Schema).CoreConfigSchema(),
	}

	for name, resource := range p.provider.ResourcesMap {
		resp.ResourceTypes[name] = providers.Schema{
			Version: int64(resource.SchemaVersion),
			Block:   resource.CoreConfigSchema(),
		}
	}

	for name, data := range p.provider.DataSourcesMap {
		resp.DataSources[name] = providers.Schema{
			Version: int64(data.SchemaVersion),
			Block:   data.CoreConfigSchema(),
		}
	}

	p.schemas = resp

	return resp
}

// PrepareProviderConfig implements the PrepareProviderConfig from
// providers.Interface. Allows the provider to validate the configuration
// values, and set or override any values with defaults.
func (p *Provider) PrepareProviderConfig(req providers.PrepareProviderConfigRequest) (resp providers.PrepareProviderConfigResponse) {
	// lookup any required, top-level attributes that are Null, and see if we
	// have a Default value available.
	configVal, err := cty.Transform(req.Config, func(path cty.Path, val cty.Value) (cty.Value, error) {
		// we're only looking for top-level attributes
		if len(path) != 1 {
			return val, nil
		}

		// nothing to do if we already have a value
		if !val.IsNull() {
			return val, nil
		}

		// get the Schema definition for this attribute
		getAttr, ok := path[0].(cty.GetAttrStep)
		// these should all exist, but just ignore anything strange
		if !ok {
			return val, nil
		}

		attrSchema := p.provider.Schema[getAttr.Name]
		// continue to ignore anything that doesn't match
		if attrSchema == nil {
			return val, nil
		}

		// this is deprecated, so don't set it
		if attrSchema.Deprecated != "" || attrSchema.Removed != "" {
			return val, nil
		}

		// find a default value if it exists
		def, err := attrSchema.DefaultValue()
		if err != nil {
			resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("error getting default for %q: %s", getAttr.Name, err))
			return val, err
		}

		// no default
		if def == nil {
			return val, nil
		}

		// create a cty.Value and make sure it's the correct type
		tmpVal := hcl2shim.HCL2ValueFromConfigValue(def)

		// helper/schema used to allow setting "" to a bool
		if val.Type() == cty.Bool && tmpVal.RawEquals(cty.StringVal("")) {
			// return a warning about the conversion
			resp.Diagnostics = resp.Diagnostics.Append("provider set empty string as default value for bool " + getAttr.Name)
			tmpVal = cty.False
		}

		val, err = convert.Convert(tmpVal, val.Type())
		if err != nil {
			resp.Diagnostics = resp.Diagnostics.Append(fmt.Errorf("error setting default for %q: %s", getAttr.Name, err))
		}

		return val, err
	})
	if err != nil {
		// any error here was already added to the diagnostics
		return resp
	}

	schema := p.getSchema()
	configVal, err = schema.Provider.Block.CoerceValue(configVal)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	// Ensure there are no nulls that will cause helper/schema to panic.
	if err := validateConfigNulls(configVal, nil); err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	resp.PreparedConfig = configVal

	return resp
}

// ValidateResourceTypeConfig implements the ValidateResourceTypeConfig from
// providers.Interface. Allows the provider to validate the resource
// configuration values.
func (p *Provider) ValidateResourceTypeConfig(req providers.ValidateResourceTypeConfigRequest) (resp providers.ValidateResourceTypeConfigResponse) {
	schemaBlock := p.getResourceSchemaBlock(req.TypeName)

	config := terraform.NewResourceConfigShimmed(req.Config, schemaBlock)

	warns, errs := p.provider.ValidateResource(req.TypeName, config)
	resp.Diagnostics = appendWarnsAndErrsToDiags(warns, errs)

	return resp
}

// ValidateDataSourceConfig implements the ValidateDataSourceConfig from providers.Interface.
// Allows the provider to validate the data source configuration values.
func (p *Provider) ValidateDataSourceConfig(req providers.ValidateDataSourceConfigRequest) (resp providers.ValidateDataSourceConfigResponse) {
	// Ensure there are no nulls that will cause helper/schema to panic.
	if err := validateConfigNulls(req.Config, nil); err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	schemaBlock := p.getDatasourceSchemaBlock(req.TypeName)
	config := terraform.NewResourceConfigShimmed(req.Config, schemaBlock)

	warns, errs := p.provider.ValidateDataSource(req.TypeName, config)
	resp.Diagnostics = appendWarnsAndErrsToDiags(warns, errs)

	return resp
}

// UpgradeResourceState implements the UpgradeResourceState from providers.Interface.
// It is called when the state loader encounters an instance state whose schema
// version is less than the one reported by the currently-used version of the
// corresponding provider, and the upgraded result is used for any further processing.
func (p *Provider) UpgradeResourceState(req providers.UpgradeResourceStateRequest) (resp providers.UpgradeResourceStateResponse) {
	res := p.provider.ResourcesMap[req.TypeName]
	schemaBlock := p.getResourceSchemaBlock(req.TypeName)

	version := int(req.Version)

	jsonMap := map[string]interface{}{}
	var err error

	switch {
	// We first need to upgrade a flatmap state if it exists.
	// There should never be both a JSON and Flatmap state in the request.
	case len(req.RawStateFlatmap) > 0:
		jsonMap, version, err = p.upgradeFlatmapState(version, req.RawStateFlatmap, res)
		if err != nil {
			resp.Diagnostics = resp.Diagnostics.Append(err)
			return resp
		}
	// if there's a JSON state, we need to decode it.
	case len(req.RawStateJSON) > 0:
		err = json.Unmarshal(req.RawStateJSON, &jsonMap)
		if err != nil {
			resp.Diagnostics = resp.Diagnostics.Append(err)
			return resp
		}
	default:
		// log.Println("[DEBUG] no state provided to upgrade")
		return resp
	}

	// complete the upgrade of the JSON states
	jsonMap, err = p.upgradeJSONState(version, jsonMap, res)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	// The provider isn't required to clean out removed fields
	p.removeAttributes(jsonMap, schemaBlock.ImpliedType())

	// now we need to turn the state into the default json representation, so
	// that it can be re-decoded using the actual schema.
	val, err := schema.JSONMapToStateValue(jsonMap, schemaBlock)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	// Now we need to make sure blocks are represented correctly, which means
	// that missing blocks are empty collections, rather than null.
	// First we need to CoerceValue to ensure that all object types match.
	val, err = schemaBlock.CoerceValue(val)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}
	// Normalize the value and fill in any missing blocks.
	val = objchange.NormalizeObjectFromLegacySDK(val, schemaBlock)

	resp.UpgradedState = val

	return resp
}

// Configure implements the Configure from providers.Interface. Configures and
// initialized the provider.
func (p *Provider) Configure(req providers.ConfigureRequest) (resp providers.ConfigureResponse) {
	p.provider.TerraformVersion = req.TerraformVersion

	// Ensure there are no nulls that will cause helper/schema to panic.
	if err := validateConfigNulls(req.Config, nil); err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	schemaBlock := schema.InternalMap(p.provider.Schema).CoreConfigSchema()
	config := terraform.NewResourceConfigShimmed(req.Config, schemaBlock)
	err := p.provider.Configure(config)
	resp.Diagnostics = resp.Diagnostics.Append(err)

	return resp
}

// Stop implements the Stop from providers.Interface. It is called when the
// provider should halt any in-flight actions.
//
// Stop should not block waiting for in-flight actions to complete. It
// should take any action it wants and return immediately acknowledging it
// has received the stop request. Terraform will not make any further API
// calls to the provider after Stop is called.
//
// The error returned, if non-nil, is assumed to mean that signaling the
// stop somehow failed and that the user should expect potentially waiting
// a longer period of time.
func (p *Provider) Stop() error {
	return p.provider.Stop()
}

// ReadResource implements the ReadResource from providers.Interface. Refreshes
// a resource and returns its current state.
func (p *Provider) ReadResource(req providers.ReadResourceRequest) (resp providers.ReadResourceResponse) {
	res := p.provider.ResourcesMap[req.TypeName]
	schemaBlock := p.getResourceSchemaBlock(req.TypeName)

	stateVal := req.PriorState

	instanceState, err := res.ShimInstanceStateFromValue(stateVal)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	resp.Private = req.Private
	private := make(map[string]interface{})
	if len(req.Private) > 0 {
		if err := json.Unmarshal(req.Private, &private); err != nil {
			resp.Diagnostics = resp.Diagnostics.Append(err)
			return resp
		}
	}
	instanceState.Meta = private

	newInstanceState, err := res.RefreshWithoutUpgrade(instanceState, p.provider.Meta())
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	if newInstanceState == nil || newInstanceState.ID == "" {
		newStateVal := cty.NullVal(schemaBlock.ImpliedType())
		resp.NewState = newStateVal

		return resp
	}

	// helper/schema should always copy the ID over, but do it again just to be safe
	newInstanceState.Attributes["id"] = newInstanceState.ID

	newStateVal, err := hcl2shim.HCL2ValueFromFlatmap(newInstanceState.Attributes, schemaBlock.ImpliedType())
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	newStateVal = normalizeNullValues(newStateVal, stateVal, false)
	newStateVal = copyTimeoutValues(newStateVal, stateVal)

	resp.NewState = newStateVal

	return resp
}

// PlanResourceChange implements the PlanResourceChange from providers.Interface.
// Takes the current state and proposed state of a resource, and returns the
// planned final state.
func (p *Provider) PlanResourceChange(req providers.PlanResourceChangeRequest) (resp providers.PlanResourceChangeResponse) {
	// This is a signal to Terraform Core that we're doing the best we can to
	// shim the legacy type system of the SDK onto the Terraform type system
	// but we need it to cut us some slack. This setting should not be taken
	// forward to any new SDK implementations, since setting it prevents us
	// from catching certain classes of provider bug that can lead to
	// confusing downstream errors.
	resp.LegacyTypeSystem = true

	res := p.provider.ResourcesMap[req.TypeName]
	schemaBlock := p.getResourceSchemaBlock(req.TypeName)

	priorStateVal := req.PriorState

	create := priorStateVal.IsNull()

	proposedNewStateVal := req.ProposedNewState

	// We don't usually plan destroys, but this can return early in any case.
	if proposedNewStateVal.IsNull() {
		resp.PlannedState = req.ProposedNewState
		resp.PlannedPrivate = req.PriorPrivate
		return resp
	}

	info := &terraform.InstanceInfo{
		Type: req.TypeName,
	}

	priorState, err := res.ShimInstanceStateFromValue(priorStateVal)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}
	priorPrivate := make(map[string]interface{})
	if len(req.PriorPrivate) > 0 {
		if err := json.Unmarshal(req.PriorPrivate, &priorPrivate); err != nil {
			resp.Diagnostics = resp.Diagnostics.Append(err)
			return resp
		}
	}

	priorState.Meta = priorPrivate

	// Ensure there are no nulls that will cause helper/schema to panic.
	if err := validateConfigNulls(proposedNewStateVal, nil); err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	// turn the proposed state into a legacy configuration
	cfg := terraform.NewResourceConfigShimmed(proposedNewStateVal, schemaBlock)

	diff, err := p.provider.SimpleDiff(info, priorState, cfg)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	// if this is a new instance, we need to make sure ID is going to be computed
	if create {
		if diff == nil {
			diff = terraform.NewInstanceDiff()
		}

		diff.Attributes["id"] = &terraform.ResourceAttrDiff{
			NewComputed: true,
		}
	}

	if diff == nil || len(diff.Attributes) == 0 {
		// schema.Provider.Diff returns nil if it ends up making a diff with no
		// changes, but our new interface wants us to return an actual change
		// description that _shows_ there are no changes. This is always the
		// prior state, because we force a diff above if this is a new instance.
		resp.PlannedState = req.PriorState
		resp.PlannedPrivate = req.PriorPrivate
		return resp
	}

	if priorState == nil {
		priorState = &terraform.InstanceState{}
	}

	// now we need to apply the diff to the prior state, so get the planned state
	plannedAttrs, err := diff.Apply(priorState.Attributes, schemaBlock)

	plannedStateVal, err := hcl2shim.HCL2ValueFromFlatmap(plannedAttrs, schemaBlock.ImpliedType())
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	plannedStateVal, err = schemaBlock.CoerceValue(plannedStateVal)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	plannedStateVal = normalizeNullValues(plannedStateVal, proposedNewStateVal, false)
	plannedStateVal = copyTimeoutValues(plannedStateVal, proposedNewStateVal)

	// The old SDK code has some imprecisions that cause it to sometimes
	// generate differences that the SDK itself does not consider significant
	// but Terraform Core would. To avoid producing weird do-nothing diffs
	// in that case, we'll check if the provider as produced something we
	// think is "equivalent" to the prior state and just return the prior state
	// itself if so, thus ensuring that Terraform Core will treat this as
	// a no-op. See the docs for ValuesSDKEquivalent for some caveats on its
	// accuracy.
	forceNoChanges := false
	if hcl2shim.ValuesSDKEquivalent(priorStateVal, plannedStateVal) {
		plannedStateVal = priorStateVal
		forceNoChanges = true
	}

	// if this was creating the resource, we need to set any remaining computed
	// fields
	if create {
		plannedStateVal = setUnknowns(plannedStateVal, schemaBlock)
	}

	resp.PlannedState = plannedStateVal

	// encode any timeouts into the diff Meta
	t := &schema.ResourceTimeout{}
	if err := t.ConfigDecode(res, cfg); err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	if err := t.DiffEncode(diff); err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	// Now we need to store any NewExtra values, which are where any actual
	// StateFunc modified config fields are hidden.
	privateMap := diff.Meta
	if privateMap == nil {
		privateMap = map[string]interface{}{}
	}

	newExtra := map[string]interface{}{}

	for k, v := range diff.Attributes {
		if v.NewExtra != nil {
			newExtra[k] = v.NewExtra
		}
	}
	privateMap[newExtraKey] = newExtra

	// the Meta field gets encoded into PlannedPrivate
	plannedPrivate, err := json.Marshal(privateMap)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}
	resp.PlannedPrivate = plannedPrivate

	// collect the attributes that require instance replacement, and convert
	// them to cty.Paths.
	var requiresNew []string
	if !forceNoChanges {
		for attr, d := range diff.Attributes {
			if d.RequiresNew {
				requiresNew = append(requiresNew, attr)
			}
		}
	}

	// If anything requires a new resource already, or the "id" field indicates
	// that we will be creating a new resource, then we need to add that to
	// RequiresReplace so that core can tell if the instance is being replaced
	// even if changes are being suppressed via "ignore_changes".
	id := plannedStateVal.GetAttr("id")
	if len(requiresNew) > 0 || id.IsNull() || !id.IsKnown() {
		requiresNew = append(requiresNew, "id")
	}

	requiresReplace, err := hcl2shim.RequiresReplace(requiresNew, schemaBlock.ImpliedType())
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	// convert these to the protocol structures
	for _, p := range requiresReplace {
		resp.RequiresReplace = append(resp.RequiresReplace, p)
	}

	return resp
}

// ApplyResourceChange implements the ApplyResourceChange from providers.Interface.
// Takes the planned state for a resource, which may yet contain unknown computed
// values, and applies the changes returning the final state.
func (p *Provider) ApplyResourceChange(req providers.ApplyResourceChangeRequest) (resp providers.ApplyResourceChangeResponse) {
	resp.NewState = req.PriorState

	res := p.provider.ResourcesMap[req.TypeName]
	schemaBlock := p.getResourceSchemaBlock(req.TypeName)

	priorStateVal := req.PriorState
	plannedStateVal := req.PlannedState

	info := &terraform.InstanceInfo{
		Type: req.TypeName,
	}

	priorState, err := res.ShimInstanceStateFromValue(priorStateVal)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	private := make(map[string]interface{})
	if len(req.PlannedPrivate) > 0 {
		if err := json.Unmarshal(req.PlannedPrivate, &private); err != nil {
			resp.Diagnostics = resp.Diagnostics.Append(err)
			return resp
		}
	}

	var diff *terraform.InstanceDiff
	destroy := false

	// a null state means we are destroying the instance
	if plannedStateVal.IsNull() {
		destroy = true
		diff = &terraform.InstanceDiff{
			Attributes: make(map[string]*terraform.ResourceAttrDiff),
			Meta:       make(map[string]interface{}),
			Destroy:    true,
		}
	} else {
		diff, err = schema.DiffFromValues(priorStateVal, plannedStateVal, stripResourceModifiers(res))
		if err != nil {
			resp.Diagnostics = resp.Diagnostics.Append(err)
			return resp
		}
	}

	if diff == nil {
		diff = &terraform.InstanceDiff{
			Attributes: make(map[string]*terraform.ResourceAttrDiff),
			Meta:       make(map[string]interface{}),
		}
	}

	// add NewExtra Fields that may have been stored in the private data
	if newExtra := private[newExtraKey]; newExtra != nil {
		for k, v := range newExtra.(map[string]interface{}) {
			d := diff.Attributes[k]

			if d == nil {
				d = &terraform.ResourceAttrDiff{}
			}

			d.NewExtra = v
			diff.Attributes[k] = d
		}
	}

	if private != nil {
		diff.Meta = private
	}

	for k, d := range diff.Attributes {
		// We need to turn off any RequiresNew. There could be attributes
		// without changes in here inserted by helper/schema, but if they have
		// RequiresNew then the state will be dropped from the ResourceData.
		d.RequiresNew = false

		// Check that any "removed" attributes that don't actually exist in the
		// prior state, or helper/schema will confuse itself
		if d.NewRemoved {
			if _, ok := priorState.Attributes[k]; !ok {
				delete(diff.Attributes, k)
			}
		}
	}

	newInstanceState, err := p.provider.Apply(info, priorState, diff)
	// we record the error here, but continue processing any returned state.
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
	}
	newStateVal := cty.NullVal(schemaBlock.ImpliedType())

	// Always return a null value for destroy.
	// While this is usually indicated by a nil state, check for missing ID or
	// attributes in the case of a provider failure.
	if destroy || newInstanceState == nil || newInstanceState.Attributes == nil || newInstanceState.ID == "" {
		resp.NewState = newStateVal
		return resp
	}

	// We keep the null val if we destroyed the resource, otherwise build the
	// entire object, even if the new state was nil.
	newStateVal, err = schema.StateValueFromInstanceState(newInstanceState, schemaBlock.ImpliedType())
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	newStateVal = normalizeNullValues(newStateVal, plannedStateVal, true)
	newStateVal = copyTimeoutValues(newStateVal, plannedStateVal)

	resp.NewState = newStateVal

	meta, err := json.Marshal(newInstanceState.Meta)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}
	resp.Private = meta

	// This is a signal to Terraform Core that we're doing the best we can to
	// shim the legacy type system of the SDK onto the Terraform type system
	// but we need it to cut us some slack. This setting should not be taken
	// forward to any new SDK implementations, since setting it prevents us
	// from catching certain classes of provider bug that can lead to
	// confusing downstream errors.
	resp.LegacyTypeSystem = true

	return resp
}

// ImportResourceState implements the ImportResourceState from providers.Interface.
// Requests that the given resource be imported.
func (p *Provider) ImportResourceState(req providers.ImportResourceStateRequest) (resp providers.ImportResourceStateResponse) {
	info := &terraform.InstanceInfo{
		Type: req.TypeName,
	}

	newInstanceStates, err := p.provider.ImportState(info, req.ID)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	for _, is := range newInstanceStates {
		// copy the ID again just to be sure it wasn't missed
		is.Attributes["id"] = is.ID

		resourceType := is.Ephemeral.Type
		if resourceType == "" {
			resourceType = req.TypeName
		}

		schemaBlock := p.getResourceSchemaBlock(resourceType)
		newStateVal, err := hcl2shim.HCL2ValueFromFlatmap(is.Attributes, schemaBlock.ImpliedType())
		if err != nil {
			resp.Diagnostics = resp.Diagnostics.Append(err)
			return resp
		}

		// Normalize the value and fill in any missing blocks.
		newStateVal = objchange.NormalizeObjectFromLegacySDK(newStateVal, schemaBlock)

		meta, err := json.Marshal(is.Meta)
		if err != nil {
			resp.Diagnostics = resp.Diagnostics.Append(err)
			return resp
		}

		resource := providers.ImportedResource{
			TypeName: resourceType,
			Private:  meta,
			State:    newStateVal,
		}

		resp.ImportedResources = append(resp.ImportedResources, resource)
	}

	return resp
}

// ReadDataSource implements the ReadDataSource from providers.Interface.
// Returns the data source's current state.
func (p *Provider) ReadDataSource(req providers.ReadDataSourceRequest) (resp providers.ReadDataSourceResponse) {
	// Ensure there are no nulls that will cause helper/schema to panic.
	if err := validateConfigNulls(req.Config, nil); err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	schemaBlock := p.getDatasourceSchemaBlock(req.TypeName)
	config := terraform.NewResourceConfigShimmed(req.Config, schemaBlock)

	info := &terraform.InstanceInfo{
		Type: req.TypeName,
	}

	// we need to still build the diff separately with the Read method to match
	// the old behavior
	diff, err := p.provider.ReadDataDiff(info, config)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	// now we can get the new complete data source
	newInstanceState, err := p.provider.ReadDataApply(info, diff)
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}

	newStateVal, err := schema.StateValueFromInstanceState(newInstanceState, schemaBlock.ImpliedType())
	if err != nil {
		resp.Diagnostics = resp.Diagnostics.Append(err)
		return resp
	}
	newStateVal = copyTimeoutValues(newStateVal, req.Config)

	resp.State = newStateVal

	return resp
}

// Close implements the Close from providers.Interface. It's to shuts down the
// plugin process but this is not a plugin, so nothing is done here
func (p *Provider) Close() error {
	return nil
}

// Internal methods
// =============================================================================

// getSchema is used internally to get the saved provider schema.  The schema
// should have already been fetched from the provider, but we have to
// synchronize access to avoid being called concurrently with GetSchema.
func (p *Provider) getSchema() providers.GetSchemaResponse {
	p.mu.Lock()
	// unlock inline in case GetSchema needs to be called
	if p.schemas.Provider.Block != nil {
		p.mu.Unlock()
		return p.schemas
	}
	p.mu.Unlock()

	schemas := p.GetSchema()
	if schemas.Diagnostics.HasErrors() {
		panic(schemas.Diagnostics.Err())
	}

	return schemas
}

// validateConfigNulls checks a config value for unsupported nulls before
// attempting to shim the value. While null values can mostly be ignored in the
// configuration, since they're not supported in HCL1, the case where a null
// appears in a list-like attribute (list, set, tuple) will present a nil value
// to helper/schema which can panic. Return an error to the user in this case,
// indicating the attribute with the null value.
func validateConfigNulls(v cty.Value, path cty.Path) []tfdiags.Diagnostic {
	var diags []tfdiags.Diagnostic
	if v.IsNull() || !v.IsKnown() {
		return diags
	}

	switch {
	case v.Type().IsListType() || v.Type().IsSetType() || v.Type().IsTupleType():
		it := v.ElementIterator()
		for it.Next() {
			kv, ev := it.Element()
			if ev.IsNull() {
				// if this is a set, the kv is also going to be null which
				// isn't a valid path element, so we can't append it to the
				// diagnostic.
				p := path
				if !kv.IsNull() {
					p = append(p, cty.IndexStep{Key: kv})
				}

				newDiag := tfdiags.AttributeValue(
					tfdiags.Error,
					"Null value found in list",
					"Null values are not allowed for this attribute value.",
					p,
				)

				diags = append(diags, newDiag)
				continue
			}

			d := validateConfigNulls(ev, append(path, cty.IndexStep{Key: kv}))
			diags = append(diags, d...)
		}

	case v.Type().IsMapType() || v.Type().IsObjectType():
		it := v.ElementIterator()
		for it.Next() {
			kv, ev := it.Element()
			var step cty.PathStep
			switch {
			case v.Type().IsMapType():
				step = cty.IndexStep{Key: kv}
			case v.Type().IsObjectType():
				step = cty.GetAttrStep{Name: kv.AsString()}
			}
			d := validateConfigNulls(ev, append(path, step))
			diags = append(diags, d...)
		}
	}

	return diags
}

func (p *Provider) getResourceSchemaBlock(name string) *configschema.Block {
	res := p.provider.ResourcesMap[name]
	return res.CoreConfigSchema()
}

func appendWarnsAndErrsToDiags(warns []string, errs []error) (diags []tfdiags.Diagnostic) {
	for _, w := range warns {
		newDiag := tfdiags.WholeContainingBody(tfdiags.Warning, w, w)
		diags = append(diags, newDiag)
	}

	for _, e := range errs {
		newDiag := tfdiags.WholeContainingBody(tfdiags.Error, e.Error(), e.Error())
		diags = append(diags, newDiag)
	}

	return diags
}

func (p *Provider) getDatasourceSchemaBlock(name string) *configschema.Block {
	dat := p.provider.DataSourcesMap[name]
	return dat.CoreConfigSchema()
}

// upgradeFlatmapState takes a legacy flatmap state, upgrades it using Migrate
// state if necessary, and converts it to the new JSON state format decoded as a
// map[string]interface{}.
// upgradeFlatmapState returns the json map along with the corresponding schema
// version.
func (p *Provider) upgradeFlatmapState(version int, m map[string]string, res *schema.Resource) (map[string]interface{}, int, error) {
	// this will be the version we've upgraded so, defaulting to the given
	// version in case no migration was called.
	upgradedVersion := version

	// first determine if we need to call the legacy MigrateState func
	requiresMigrate := version < res.SchemaVersion

	schemaType := res.CoreConfigSchema().ImpliedType()

	// if there are any StateUpgraders, then we need to only compare
	// against the first version there
	if len(res.StateUpgraders) > 0 {
		requiresMigrate = version < res.StateUpgraders[0].Version
	}

	if requiresMigrate && res.MigrateState == nil {
		// Providers were previously allowed to bump the version
		// without declaring MigrateState.
		// If there are further upgraders, then we've only updated that far.
		if len(res.StateUpgraders) > 0 {
			schemaType = res.StateUpgraders[0].Type
			upgradedVersion = res.StateUpgraders[0].Version
		}
	} else if requiresMigrate {
		is := &terraform.InstanceState{
			ID:         m["id"],
			Attributes: m,
			Meta: map[string]interface{}{
				"schema_version": strconv.Itoa(version),
			},
		}

		is, err := res.MigrateState(version, is, p.provider.Meta())
		if err != nil {
			return nil, 0, err
		}

		// re-assign the map in case there was a copy made, making sure to keep
		// the ID
		m := is.Attributes
		m["id"] = is.ID

		// if there are further upgraders, then we've only updated that far
		if len(res.StateUpgraders) > 0 {
			schemaType = res.StateUpgraders[0].Type
			upgradedVersion = res.StateUpgraders[0].Version
		}
	} else {
		// the schema version may be newer than the MigrateState functions
		// handled and older than the current, but still stored in the flatmap
		// form. If that's the case, we need to find the correct schema type to
		// convert the state.
		for _, upgrader := range res.StateUpgraders {
			if upgrader.Version == version {
				schemaType = upgrader.Type
				break
			}
		}
	}

	// now we know the state is up to the latest version that handled the
	// flatmap format state. Now we can upgrade the format and continue from
	// there.
	newConfigVal, err := hcl2shim.HCL2ValueFromFlatmap(m, schemaType)
	if err != nil {
		return nil, 0, err
	}

	jsonMap, err := schema.StateValueToJSONMap(newConfigVal, schemaType)
	return jsonMap, upgradedVersion, err
}

func (p *Provider) upgradeJSONState(version int, m map[string]interface{}, res *schema.Resource) (map[string]interface{}, error) {
	var err error

	for _, upgrader := range res.StateUpgraders {
		if version != upgrader.Version {
			continue
		}

		m, err = upgrader.Upgrade(m, p.provider.Meta())
		if err != nil {
			return nil, err
		}
		version++
	}

	return m, nil
}

// Remove any attributes no longer present in the schema, so that the json can
// be correctly decoded.
func (p *Provider) removeAttributes(v interface{}, ty cty.Type) {
	// we're only concerned with finding maps that corespond to object
	// attributes
	switch v := v.(type) {
	case []interface{}:
		// If these aren't blocks the next call will be a noop
		if ty.IsListType() || ty.IsSetType() {
			eTy := ty.ElementType()
			for _, eV := range v {
				p.removeAttributes(eV, eTy)
			}
		}
		return
	case map[string]interface{}:
		// map blocks aren't yet supported, but handle this just in case
		if ty.IsMapType() {
			eTy := ty.ElementType()
			for _, eV := range v {
				p.removeAttributes(eV, eTy)
			}
			return
		}

		if ty == cty.DynamicPseudoType {
			log.Printf("[DEBUG] ignoring dynamic block: %#v\n", v)
			return
		}

		if !ty.IsObjectType() {
			// This shouldn't happen, and will fail to decode further on, so
			// there's no need to handle it here.
			log.Printf("[WARN] unexpected type %#v for map in json state", ty)
			return
		}

		attrTypes := ty.AttributeTypes()
		for attr, attrV := range v {
			attrTy, ok := attrTypes[attr]
			if !ok {
				log.Printf("[DEBUG] attribute %q no longer present in schema", attr)
				delete(v, attr)
				continue
			}

			p.removeAttributes(attrV, attrTy)
		}
	}
}

// Zero values and empty containers may be interchanged by the apply process.
// When there is a discrepency between src and dst value being null or empty,
// prefer the src value. This takes a little more liberty with set types, since
// we can't correlate modified set values. In the case of sets, if the src set
// was wholly known we assume the value was correctly applied and copy that
// entirely to the new value.
// While apply prefers the src value, during plan we prefer dst whenever there
// is an unknown or a set is involved, since the plan can alter the value
// however it sees fit. This however means that a CustomizeDiffFunction may not
// be able to change a null to an empty value or vice versa, but that should be
// very uncommon nor was it reliable before 0.12 either.
func normalizeNullValues(dst, src cty.Value, apply bool) cty.Value {
	ty := dst.Type()
	if !src.IsNull() && !src.IsKnown() {
		// Return src during plan to retain unknown interpolated placeholders,
		// which could be lost if we're only updating a resource. If this is a
		// read scenario, then there shouldn't be any unknowns at all.
		if dst.IsNull() && !apply {
			return src
		}
		return dst
	}

	// Handle null/empty changes for collections during apply.
	// A change between null and empty values prefers src to make sure the state
	// is consistent between plan and apply.
	if ty.IsCollectionType() && apply {
		dstEmpty := !dst.IsNull() && dst.IsKnown() && dst.LengthInt() == 0
		srcEmpty := !src.IsNull() && src.IsKnown() && src.LengthInt() == 0

		if (src.IsNull() && dstEmpty) || (srcEmpty && dst.IsNull()) {
			return src
		}
	}

	// check the invariants that we need below, to ensure we are working with
	// non-null and known values.
	if src.IsNull() || !src.IsKnown() || !dst.IsKnown() {
		return dst
	}

	switch {
	case ty.IsMapType(), ty.IsObjectType():
		var dstMap map[string]cty.Value
		if !dst.IsNull() {
			dstMap = dst.AsValueMap()
		}
		if dstMap == nil {
			dstMap = map[string]cty.Value{}
		}

		srcMap := src.AsValueMap()
		for key, v := range srcMap {
			dstVal, ok := dstMap[key]
			if !ok && apply && ty.IsMapType() {
				// don't transfer old map values to dst during apply
				continue
			}

			if dstVal == cty.NilVal {
				if !apply && ty.IsMapType() {
					// let plan shape this map however it wants
					continue
				}
				dstVal = cty.NullVal(v.Type())
			}

			dstMap[key] = normalizeNullValues(dstVal, v, apply)
		}

		// you can't call MapVal/ObjectVal with empty maps, but nothing was
		// copied in anyway. If the dst is nil, and the src is known, assume the
		// src is correct.
		if len(dstMap) == 0 {
			if dst.IsNull() && src.IsWhollyKnown() && apply {
				return src
			}
			return dst
		}

		if ty.IsMapType() {
			// helper/schema will populate an optional+computed map with
			// unknowns which we have to fixup here.
			// It would be preferable to simply prevent any known value from
			// becoming unknown, but concessions have to be made to retain the
			// broken legacy behavior when possible.
			for k, srcVal := range srcMap {
				if !srcVal.IsNull() && srcVal.IsKnown() {
					dstVal, ok := dstMap[k]
					if !ok {
						continue
					}

					if !dstVal.IsNull() && !dstVal.IsKnown() {
						dstMap[k] = srcVal
					}
				}
			}

			return cty.MapVal(dstMap)
		}

		return cty.ObjectVal(dstMap)

	case ty.IsSetType():
		// If the original was wholly known, then we expect that is what the
		// provider applied. The apply process loses too much information to
		// reliably re-create the set.
		if src.IsWhollyKnown() && apply {
			return src
		}

	case ty.IsListType(), ty.IsTupleType():
		// If the dst is null, and the src is known, then we lost an empty value
		// so take the original.
		if dst.IsNull() {
			if src.IsWhollyKnown() && src.LengthInt() == 0 && apply {
				return src
			}

			// if dst is null and src only contains unknown values, then we lost
			// those during a read or plan.
			if !apply && !src.IsNull() {
				allUnknown := true
				for _, v := range src.AsValueSlice() {
					if v.IsKnown() {
						allUnknown = false
						break
					}
				}
				if allUnknown {
					return src
				}
			}

			return dst
		}

		// if the lengths are identical, then iterate over each element in succession.
		srcLen := src.LengthInt()
		dstLen := dst.LengthInt()
		if srcLen == dstLen && srcLen > 0 {
			srcs := src.AsValueSlice()
			dsts := dst.AsValueSlice()

			for i := 0; i < srcLen; i++ {
				dsts[i] = normalizeNullValues(dsts[i], srcs[i], apply)
			}

			if ty.IsTupleType() {
				return cty.TupleVal(dsts)
			}
			return cty.ListVal(dsts)
		}

	case ty == cty.String:
		// The legacy SDK should not be able to remove a value during plan or
		// apply, however we are only going to overwrite this if the source was
		// an empty string, since that is what is often equated with unset and
		// lost in the diff process.
		if dst.IsNull() && src.AsString() == "" {
			return src
		}
	}

	return dst
}

// helper/schema throws away timeout values from the config and stores them in
// the Private/Meta fields. we need to copy those values into the planned state
// so that core doesn't see a perpetual diff with the timeout block.
func copyTimeoutValues(to cty.Value, from cty.Value) cty.Value {
	// if `to` is null we are planning to remove it altogether.
	if to.IsNull() {
		return to
	}
	toAttrs := to.AsValueMap()
	// We need to remove the key since the hcl2shims will add a non-null block
	// because we can't determine if a single block was null from the flatmapped
	// values. This needs to conform to the correct schema for marshaling, so
	// change the value to null rather than deleting it from the object map.
	timeouts, ok := toAttrs[schema.TimeoutsConfigKey]
	if ok {
		toAttrs[schema.TimeoutsConfigKey] = cty.NullVal(timeouts.Type())
	}

	// if from is null then there are no timeouts to copy
	if from.IsNull() {
		return cty.ObjectVal(toAttrs)
	}

	fromAttrs := from.AsValueMap()
	timeouts, ok = fromAttrs[schema.TimeoutsConfigKey]

	// timeouts shouldn't be unknown, but don't copy possibly invalid values either
	if !ok || timeouts.IsNull() || !timeouts.IsWhollyKnown() {
		// no timeouts block to copy
		return cty.ObjectVal(toAttrs)
	}

	toAttrs[schema.TimeoutsConfigKey] = timeouts

	return cty.ObjectVal(toAttrs)
}

// setUnknowns takes a cty.Value, and compares it to the schema setting any null
// values which are computed to unknown.
func setUnknowns(val cty.Value, schema *configschema.Block) cty.Value {
	if !val.IsKnown() {
		return val
	}

	// If the object was null, we still need to handle the top level attributes
	// which might be computed, but we don't need to expand the blocks.
	if val.IsNull() {
		objMap := map[string]cty.Value{}
		allNull := true
		for name, attr := range schema.Attributes {
			switch {
			case attr.Computed:
				objMap[name] = cty.UnknownVal(attr.Type)
				allNull = false
			default:
				objMap[name] = cty.NullVal(attr.Type)
			}
		}

		// If this object has no unknown attributes, then we can leave it null.
		if allNull {
			return val
		}

		return cty.ObjectVal(objMap)
	}

	valMap := val.AsValueMap()
	newVals := make(map[string]cty.Value)

	for name, attr := range schema.Attributes {
		v := valMap[name]

		if attr.Computed && v.IsNull() {
			newVals[name] = cty.UnknownVal(attr.Type)
			continue
		}

		newVals[name] = v
	}

	for name, blockS := range schema.BlockTypes {
		blockVal := valMap[name]
		if blockVal.IsNull() || !blockVal.IsKnown() {
			newVals[name] = blockVal
			continue
		}

		blockValType := blockVal.Type()
		blockElementType := blockS.Block.ImpliedType()

		// This switches on the value type here, so we can correctly switch
		// between Tuples/Lists and Maps/Objects.
		switch {
		case blockS.Nesting == configschema.NestingSingle || blockS.Nesting == configschema.NestingGroup:
			// NestingSingle is the only exception here, where we treat the
			// block directly as an object
			newVals[name] = setUnknowns(blockVal, &blockS.Block)

		case blockValType.IsSetType(), blockValType.IsListType(), blockValType.IsTupleType():
			listVals := blockVal.AsValueSlice()
			newListVals := make([]cty.Value, 0, len(listVals))

			for _, v := range listVals {
				newListVals = append(newListVals, setUnknowns(v, &blockS.Block))
			}

			switch {
			case blockValType.IsSetType():
				switch len(newListVals) {
				case 0:
					newVals[name] = cty.SetValEmpty(blockElementType)
				default:
					newVals[name] = cty.SetVal(newListVals)
				}
			case blockValType.IsListType():
				switch len(newListVals) {
				case 0:
					newVals[name] = cty.ListValEmpty(blockElementType)
				default:
					newVals[name] = cty.ListVal(newListVals)
				}
			case blockValType.IsTupleType():
				newVals[name] = cty.TupleVal(newListVals)
			}

		case blockValType.IsMapType(), blockValType.IsObjectType():
			mapVals := blockVal.AsValueMap()
			newMapVals := make(map[string]cty.Value)

			for k, v := range mapVals {
				newMapVals[k] = setUnknowns(v, &blockS.Block)
			}

			switch {
			case blockValType.IsMapType():
				switch len(newMapVals) {
				case 0:
					newVals[name] = cty.MapValEmpty(blockElementType)
				default:
					newVals[name] = cty.MapVal(newMapVals)
				}
			case blockValType.IsObjectType():
				if len(newMapVals) == 0 {
					// We need to populate empty values to make a valid object.
					for attr, ty := range blockElementType.AttributeTypes() {
						newMapVals[attr] = cty.NullVal(ty)
					}
				}
				newVals[name] = cty.ObjectVal(newMapVals)
			}

		default:
			panic(fmt.Sprintf("failed to set unknown values for nested block %q:%#v", name, blockValType))
		}
	}

	return cty.ObjectVal(newVals)
}

// stripResourceModifiers takes a *schema.Resource and returns a deep copy with all
// StateFuncs and CustomizeDiffs removed. This will be used during apply to
// create a diff from a planned state where the diff modifications have already
// been applied.
func stripResourceModifiers(r *schema.Resource) *schema.Resource {
	if r == nil {
		return nil
	}
	// start with a shallow copy
	newResource := new(schema.Resource)
	*newResource = *r

	newResource.CustomizeDiff = nil
	newResource.Schema = map[string]*schema.Schema{}

	for k, s := range r.Schema {
		newResource.Schema[k] = stripSchema(s)
	}

	return newResource
}

func stripSchema(s *schema.Schema) *schema.Schema {
	if s == nil {
		return nil
	}
	// start with a shallow copy
	newSchema := new(schema.Schema)
	*newSchema = *s

	newSchema.StateFunc = nil

	switch e := newSchema.Elem.(type) {
	case *schema.Schema:
		newSchema.Elem = stripSchema(e)
	case *schema.Resource:
		newSchema.Elem = stripResourceModifiers(e)
	}

	return newSchema
}
