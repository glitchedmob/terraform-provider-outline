// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"net/http"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	resourceschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func memberTestProtocolConfig(t *testing.T, values map[string]any) *tfprotov6.DynamicValue {
	t.Helper()
	attrs := map[string]tftypes.Type{"id": tftypes.String, "group_id": tftypes.String, "user_id": tftypes.String, "permission": tftypes.String}
	fields := make(map[string]tftypes.Value)
	for key, typ := range attrs {
		fields[key] = tftypes.NewValue(typ, values[key])
	}
	value := tftypes.NewValue(tftypes.Object{AttributeTypes: attrs}, fields)
	dynamic, err := tfprotov6.NewDynamicValue(value.Type(), value)
	if err != nil {
		t.Fatal(err)
	}
	return &dynamic
}

func TestGroupMemberSchemaConfigureAndProtocolValidation(t *testing.T) {
	r := NewGroupMemberResource().(*groupMemberResource)
	var schema resource.SchemaResponse
	r.Schema(t.Context(), resource.SchemaRequest{}, &schema)
	if schema.Diagnostics.HasError() || schema.Schema.ValidateImplementation(t.Context()).HasError() || len(schema.Schema.Attributes) != 4 {
		t.Fatal(schema.Diagnostics)
	}
	id := schema.Schema.Attributes["id"].(resourceschema.StringAttribute)
	permission := schema.Schema.Attributes["permission"].(resourceschema.StringAttribute)
	if !id.Computed || id.Optional || id.Required || !permission.Computed || !permission.Optional || permission.Required || permission.Default == nil || len(permission.Validators) == 0 {
		t.Fatal("incorrect computed identity or permission/default schema")
	}
	for _, name := range []string{"group_id", "user_id"} {
		attr := schema.Schema.Attributes[name].(resourceschema.StringAttribute)
		if !attr.Required || attr.Computed || attr.Optional || len(attr.PlanModifiers) != 1 || len(attr.Validators) == 0 || attr.MarkdownDescription == "" {
			t.Fatalf("wrong pair schema: %s", name)
		}
	}
	for _, data := range []any{nil, "wrong type", (*apiClient)(nil), &apiClient{}} {
		var resp resource.ConfigureResponse
		r.Configure(t.Context(), resource.ConfigureRequest{ProviderData: data}, &resp)
		valid := data == nil
		if api, ok := data.(*apiClient); ok && api != nil {
			valid = true
			if r.api != api {
				t.Fatal("client not retained")
			}
		}
		if resp.Diagnostics.HasError() == valid {
			t.Fatal(resp.Diagnostics)
		}
	}
	for _, field := range []string{"group_id", "user_id", "permission", "id"} {
		for _, value := range []any{nil, "", "not-uuid", "00000000-0000-0000-0000-000000000000", strings.ToUpper(groupTestID), "a32c2ee6-fbde-0654-841b-0eabdc71b812", "a32c2ee6-fbde-4654-041b-0eabdc71b812", "member", "admin", tftypes.UnknownValue} {
			t.Run(field+"/"+toTestName(value), func(t *testing.T) {
				values := map[string]any{"group_id": groupTestID, "user_id": userTestID}
				values[field] = value
				valid := value == tftypes.UnknownValue
				switch field {
				case "id":
					valid = value == nil
				case "permission":
					valid = valid || value == nil || value == "member" || value == "admin"
				}
				server := providerserver.NewProtocol6(New("unit")())()
				resp, err := server.ValidateResourceConfig(t.Context(), &tfprotov6.ValidateResourceConfigRequest{TypeName: "outline_group_member", Config: memberTestProtocolConfig(t, values)})
				if err != nil || protocolHasError(resp.Diagnostics) == valid {
					t.Fatalf("valid=%t: %v %v", valid, resp, err)
				}
			})
		}
	}
}

func toTestName(value any) string {
	if value == nil {
		return "null"
	}
	if s, ok := value.(string); ok {
		return s
	}
	return "unknown"
}

func TestGroupMemberImportDelimiterAndValidation(t *testing.T) {
	id := groupTestID + "/" + userTestID
	for _, input := range []string{id, "", groupTestID, userTestID + "-" + groupTestID, groupTestID + ":" + userTestID, id + "/extra", "/" + id, id + "/", strings.ToUpper(id), strings.ReplaceAll(id, "-", ""), strings.ReplaceAll(id, "/", "%2F"), "urn:uuid:" + id, " " + id, groupTestID + "/00000000-0000-0000-0000-000000000000", "a32c2ee6-fbde-4654-041b-0eabdc71b812/" + userTestID} {
		t.Run(input, func(t *testing.T) {
			r := &groupMemberResource{api: memberTestClient(t, func(http.ResponseWriter, *http.Request) { t.Error("import must not call API") })}
			state := tfsdk.State(memberTestPlan(t, r, groupMemberModel{}))
			resp := resource.ImportStateResponse{State: state}
			r.ImportState(t.Context(), resource.ImportStateRequest{ID: input}, &resp)
			if resp.Diagnostics.HasError() != (input != id) {
				t.Fatal(resp.Diagnostics)
			}
			if input == id {
				got := memberTestState(t, resp.State)
				if got.ID.ValueString() != id || got.GroupID.ValueString() != groupTestID || got.UserID.ValueString() != userTestID || !got.Permission.IsNull() {
					t.Fatalf("import must populate pair and leave permission for Read: %+v", got)
				}
			} else if !resp.State.Raw.Equal(state.Raw) {
				t.Fatal("invalid import changed state")
			}
		})
	}
}

func TestGroupMemberProtocolPlanDefaultsIdentityAndReplacement(t *testing.T) {
	for _, change := range []string{"create", "none", "group_id", "user_id", "permission"} {
		t.Run(change, func(t *testing.T) {
			server := providerserver.NewProtocol6(New("unit")())()
			var schema resource.SchemaResponse
			NewGroupMemberResource().Schema(t.Context(), resource.SchemaRequest{}, &schema)
			typ := schema.Schema.Type().TerraformType(t.Context())
			prior := memberTestProtocolConfig(t, map[string]any{"id": groupTestID + "/" + userTestID, "group_id": groupTestID, "user_id": userTestID, "permission": "admin"})
			if change == "create" {
				null, err := tfprotov6.NewDynamicValue(typ, tftypes.NewValue(typ, nil))
				if err != nil {
					t.Fatal(err)
				}
				prior = &null
			}
			group, user := groupTestID, userTestID
			if change == "group_id" {
				group = groupTestOtherID
			}
			if change == "user_id" {
				user = userTestOtherID
			}
			config := map[string]any{"group_id": group, "user_id": user}
			if change == "permission" {
				config["permission"] = "member"
			}
			var proposedPermission any = tftypes.UnknownValue
			if change == "permission" {
				proposedPermission = "member"
			}
			resp, err := server.PlanResourceChange(t.Context(), &tfprotov6.PlanResourceChangeRequest{TypeName: "outline_group_member", PriorState: prior, Config: memberTestProtocolConfig(t, config), ProposedNewState: memberTestProtocolConfig(t, map[string]any{"id": tftypes.UnknownValue, "group_id": group, "user_id": user, "permission": proposedPermission})})
			if err != nil || protocolHasError(resp.Diagnostics) || resp.PlannedState == nil {
				t.Fatalf("plan: %v %v", resp, err)
			}
			if change == "group_id" || change == "user_id" {
				if len(resp.RequiresReplace) != 1 || !resp.RequiresReplace[0].Equal(tftypes.NewAttributePath().WithAttributeName(change)) {
					t.Fatalf("wrong replacement paths: %v", resp.RequiresReplace)
				}
			} else if len(resp.RequiresReplace) != 0 {
				t.Fatalf("unexpected replacement: %v", resp.RequiresReplace)
			}
			value, err := resp.PlannedState.Unmarshal(typ)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]tftypes.Value
			if err := value.As(&fields); err != nil {
				t.Fatal(err)
			}
			var id, permission string
			if err := fields["id"].As(&id); err != nil || id != group+"/"+user {
				t.Fatalf("computed identity=%q %v", id, err)
			}
			if err := fields["permission"].As(&permission); err != nil || permission != "member" {
				t.Fatalf("default must reconcile omitted imported admin permission to member: %q %v", permission, err)
			}
		})
	}
}

func TestGroupMemberInvalidStatePairNeverCallsAPI(t *testing.T) {
	for _, field := range []string{"group_id", "user_id", "id"} {
		model := memberTestModel()
		switch field {
		case "group_id":
			model.GroupID = types.StringValue("not-a-group")
		case "user_id":
			model.UserID = types.StringValue("not-a-user")
		case "id":
			model.ID = types.StringValue(groupTestOtherID + "/" + userTestID)
		}
		r := &groupMemberResource{api: memberTestClient(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid state called API") })}
		state := tfsdk.State(memberTestPlan(t, r, model))
		read := resource.ReadResponse{State: state}
		r.Read(t.Context(), resource.ReadRequest{State: state}, &read)
		updated := resource.UpdateResponse{State: state}
		r.Update(t.Context(), resource.UpdateRequest{State: state, Plan: memberTestPlan(t, r, model)}, &updated)
		deleted := resource.DeleteResponse{State: state}
		r.Delete(t.Context(), resource.DeleteRequest{State: state}, &deleted)
		if !read.Diagnostics.HasError() || !updated.Diagnostics.HasError() || !deleted.Diagnostics.HasError() || !read.State.Raw.Equal(state.Raw) {
			t.Fatalf("invalid %s changed state or lacked errors", field)
		}
	}
}
