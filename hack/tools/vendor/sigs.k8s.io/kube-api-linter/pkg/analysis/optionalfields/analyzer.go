/*
Copyright 2025 The Kubernetes Authors.

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
package optionalfields

import (
	"errors"
	"fmt"
	"go/ast"
	"strings"

	"golang.org/x/tools/go/analysis"
	kalerrors "sigs.k8s.io/kube-api-linter/pkg/analysis/errors"
	"sigs.k8s.io/kube-api-linter/pkg/analysis/helpers/extractjsontags"
	"sigs.k8s.io/kube-api-linter/pkg/analysis/helpers/inspector"
	markershelper "sigs.k8s.io/kube-api-linter/pkg/analysis/helpers/markers"
	"sigs.k8s.io/kube-api-linter/pkg/analysis/utils"
	"sigs.k8s.io/kube-api-linter/pkg/markers"
)

const (
	name = "optionalfields"
)

func init() {
	markershelper.DefaultRegistry().Register(
		markers.OptionalMarker,
		markers.RequiredMarker,
		markers.KubebuilderOptionalMarker,
		markers.KubebuilderRequiredMarker,
		markers.KubebuilderMinItemsMarker,
		markers.KubebuilderMinLengthMarker,
		markers.KubebuilderMinPropertiesMarker,
		markers.KubebuilderMinimumMarker,
		markers.KubebuilderEnumMarker,
	)
}

var (
	errMarkerMissingValue = errors.New("marker does not have a value")
)

type analyzer struct {
	pointerPolicy     OptionalFieldsPointerPolicy
	pointerPreference OptionalFieldsPointerPreference
	omitEmptyPolicy   OptionalFieldsOmitEmptyPolicy
}

// newAnalyzer creates a new analyzer.
func newAnalyzer(cfg *OptionalFieldsConfig) *analysis.Analyzer {
	if cfg == nil {
		cfg = &OptionalFieldsConfig{}
	}

	defaultConfig(cfg)

	a := &analyzer{
		pointerPolicy:     cfg.Pointers.Policy,
		pointerPreference: cfg.Pointers.Preference,
		omitEmptyPolicy:   cfg.OmitEmpty.Policy,
	}

	return &analysis.Analyzer{
		Name: name,
		Doc: `Checks all optional fields comply with the configured policy.
		Depending on the configuration, this may include checking for the presence of the omitempty tag or
		whether the field is a pointer.
		For structs, this includes checking that if the field is marked as optional, it should be a pointer when it has omitempty.
		Where structs include required fields, they must be a pointer when they themselves are optional.
		`,
		Run:      a.run,
		Requires: []*analysis.Analyzer{inspector.Analyzer, extractjsontags.Analyzer},
	}
}

func (a *analyzer) run(pass *analysis.Pass) (any, error) {
	inspect, ok := pass.ResultOf[inspector.Analyzer].(inspector.Inspector)
	if !ok {
		return nil, kalerrors.ErrCouldNotGetInspector
	}

	inspect.InspectFields(func(field *ast.Field, stack []ast.Node, jsonTagInfo extractjsontags.FieldTagInfo, markersAccess markershelper.Markers) {
		a.checkField(pass, field, markersAccess, jsonTagInfo)
	})

	return nil, nil //nolint:nilnil
}

func (a *analyzer) checkField(pass *analysis.Pass, field *ast.Field, markersAccess markershelper.Markers, jsonTags extractjsontags.FieldTagInfo) {
	if field == nil || len(field.Names) == 0 {
		return
	}

	fieldMarkers := markersAccess.FieldMarkers(field)

	fieldName := field.Names[0].Name

	if !isFieldOptional(fieldMarkers) {
		// The field is not marked optional, so we don't need to check it.
		return
	}

	if field.Type == nil {
		// The field has no type? We can't check if it's a pointer.
		return
	}

	a.checkFieldProperties(pass, field, fieldName, markersAccess, jsonTags)
}

func defaultConfig(cfg *OptionalFieldsConfig) {
	if cfg.Pointers.Policy == "" {
		cfg.Pointers.Policy = OptionalFieldsPointerPolicySuggestFix
	}

	if cfg.Pointers.Preference == "" {
		cfg.Pointers.Preference = OptionalFieldsPointerPreferenceAlways
	}

	if cfg.OmitEmpty.Policy == "" {
		cfg.OmitEmpty.Policy = OptionalFieldsOmitEmptyPolicySuggestFix
	}
}

func (a *analyzer) checkFieldProperties(pass *analysis.Pass, field *ast.Field, fieldName string, markersAccess markershelper.Markers, jsonTags extractjsontags.FieldTagInfo) {
	hasValidZeroValue, completeValidation := isZeroValueValid(pass, field, field.Type, markersAccess)
	hasOmitEmpty := jsonTags.OmitEmpty
	isPointer, underlying := isStarExpr(field.Type)
	isStruct := isStructType(pass, field.Type)

	if a.pointerPreference == OptionalFieldsPointerPreferenceAlways {
		// The field must always be a pointer, pointers require omitempty, so enforce that too.
		a.handleFieldShouldBePointer(pass, field, fieldName, isPointer, underlying)
		a.handleFieldShouldHaveOmitEmpty(pass, field, fieldName, hasOmitEmpty, jsonTags)

		return
	}

	// The pointer preference is now when required.

	if a.omitEmptyPolicy != OptionalFieldsOmitEmptyPolicyIgnore || hasOmitEmpty {
		// If we require omitempty, or the field has omitempty, we can check the field properties based on it being an omitempty field.
		a.checkFieldPropertiesWithOmitEmptyRequired(pass, field, fieldName, jsonTags, underlying, hasOmitEmpty, hasValidZeroValue, completeValidation, isPointer, isStruct)
	} else {
		// The field does not have omitempty, and does not require it.
		a.checkFieldPropertiesWithoutOmitEmpty(pass, field, fieldName, jsonTags, underlying, hasValidZeroValue, completeValidation, isPointer, isStruct)
	}
}

func (a *analyzer) checkFieldPropertiesWithOmitEmptyRequired(pass *analysis.Pass, field *ast.Field, fieldName string, jsonTags extractjsontags.FieldTagInfo, underlying ast.Expr, hasOmitEmpty, hasValidZeroValue, completeValidation, isPointer, isStruct bool) {
	// In this case, we should always add the omitempty if it isn't present.
	a.handleFieldShouldHaveOmitEmpty(pass, field, fieldName, hasOmitEmpty, jsonTags)

	switch {
	case hasValidZeroValue && !completeValidation:
		a.handleIncompleteFieldValidation(pass, field, fieldName, isPointer, underlying)
		fallthrough // Since it's a valid zero value, we should still enforce the pointer.
	case hasValidZeroValue, isStruct:
		// The field validation infers that the zero value is valid, the field needs to be a pointer.
		// Optional structs with omitempty should always be pointers, else they won't actually be omitted.
		a.handleFieldShouldBePointer(pass, field, fieldName, isPointer, underlying)
	case !hasValidZeroValue && completeValidation && !isStruct:
		// The validation is fully complete, and the zero value is not valid, so we don't need a pointer.
		a.handleFieldShouldNotBePointer(pass, field, fieldName, isPointer, "field %s is optional and does not allow the zero value. The field does not need to be a pointer.")
	}
}

func (a *analyzer) checkFieldPropertiesWithoutOmitEmpty(pass *analysis.Pass, field *ast.Field, fieldName string, jsonTags extractjsontags.FieldTagInfo, underlying ast.Expr, hasValidZeroValue, completeValidation, isPointer, isStruct bool) {
	switch {
	case hasValidZeroValue:
		// The field is not omitempty, and the zero value is valid, the field does not need to be a pointer.
		a.handleFieldShouldNotBePointer(pass, field, fieldName, isPointer, "field %s is optional, without omitempty and allows the zero value. The field does not need to be a pointer.")
	case !hasValidZeroValue:
		// The zero value would not be accepted, so the field needs to have omitempty.
		// Force the omitempty policy to suggest a fix. We can only get to this function when the policy is configured to Ignore.
		// Since we absolutely have to add the omitempty tag, we can report it as a suggestion.
		reportShouldAddOmitEmpty(pass, field, OptionalFieldsOmitEmptyPolicySuggestFix, fieldName, "field %s is optional and does not allow the zero value. It must have the omitempty tag.", jsonTags)
		// Once it has the omitempty tag, it will also need to be a pointer in some cases.
		// Now handle it as if it had the omitempty already.
		// We already handle the omitempty tag above, so force the `hasOmitEmpty` to true.
		a.checkFieldPropertiesWithOmitEmptyRequired(pass, field, fieldName, jsonTags, underlying, true, hasValidZeroValue, completeValidation, isPointer, isStruct)
	}
}

func (a *analyzer) handleFieldShouldBePointer(pass *analysis.Pass, field *ast.Field, fieldName string, isPointer bool, underlying ast.Expr) {
	if isPointerType(pass, underlying) {
		if isPointer {
			switch a.pointerPolicy {
			case OptionalFieldsPointerPolicySuggestFix:
				reportShouldRemovePointer(pass, field, OptionalFieldsPointerPolicySuggestFix, fieldName, "field %s is optional but the underlying type does not need to be a pointer. The pointer should be removed.")
			case OptionalFieldsPointerPolicyWarn:
				pass.Reportf(field.Pos(), "field %s is optional but the underlying type does not need to be a pointer. The pointer should be removed.", fieldName)
			}
		}

		return
	}

	if isPointer {
		return
	}

	switch a.pointerPolicy {
	case OptionalFieldsPointerPolicySuggestFix:
		reportShouldAddPointer(pass, field, OptionalFieldsPointerPolicySuggestFix, fieldName, "field %s is optional and should be a pointer")
	case OptionalFieldsPointerPolicyWarn:
		pass.Reportf(field.Pos(), "field %s is optional and should be a pointer", fieldName)
	}
}

func (a *analyzer) handleFieldShouldNotBePointer(pass *analysis.Pass, field *ast.Field, fieldName string, isPointer bool, message string) {
	if !isPointer {
		return
	}

	reportShouldRemovePointer(pass, field, a.pointerPolicy, fieldName, message)
}

func (a *analyzer) handleFieldShouldHaveOmitEmpty(pass *analysis.Pass, field *ast.Field, fieldName string, hasOmitEmpty bool, jsonTags extractjsontags.FieldTagInfo) {
	if hasOmitEmpty {
		return
	}

	reportShouldAddOmitEmpty(pass, field, a.omitEmptyPolicy, fieldName, "field %s is optional and should have the omitempty tag", jsonTags)
}

func (a *analyzer) handleIncompleteFieldValidation(pass *analysis.Pass, field *ast.Field, fieldName string, isPointer bool, underlying ast.Expr) {
	if isPointer || isPointerType(pass, underlying) {
		// Don't warn them if the field is already a pointer.
		// If they change the validation then they'll fall into the correct logic for categorizing the field.
		// When the field is a pointer type (e.g. map, array), we don't need to warn them either as they should not make these fields pointers.
		return
	}

	zeroValue := getTypedZeroValue(pass, underlying)
	validationHint := getTypedValidationHint(pass, underlying)

	pass.Reportf(field.Pos(), "field %s is optional and has a valid zero value (%s), but the validation is not complete (e.g. %s). The field should be a pointer to allow the zero value to be set. If the zero value is not a valid use case, complete the validation and remove the pointer.", fieldName, zeroValue, validationHint)
}

// getTypedZeroValue returns the zero value for a given type as a string representation.
func getTypedZeroValue(pass *analysis.Pass, expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return getIdentZeroValue(pass, t)
	case *ast.StructType:
		return getStructZeroValue(pass, t)
	case *ast.ArrayType:
		return "[]"
	case *ast.MapType:
		return "{}"
	default:
		return ""
	}
}

// getIdentZeroValue returns the zero value for a given identifier as a string representation.
// Where the ident is an alias for a type, it will look up the type spec to get the underlying type
// and return the zero value for that type.
func getIdentZeroValue(pass *analysis.Pass, ident *ast.Ident) string {
	switch {
	case isIntegerIdent(ident):
		return "0"
	case isStringIdent(ident):
		return `""`
	case isBoolIdent(ident):
		return "false"
	case isFloatIdent(ident):
		return "0.0"
	}

	typeSpec, ok := utils.LookupTypeSpec(pass, ident)
	if !ok {
		return ""
	}

	return getTypedZeroValue(pass, typeSpec.Type)
}

// getStructZeroValue returns the zero value for a struct type as a string representation.
// It constructs a json-like representation of the struct's zero value,
// including only the fields that are not omitted (i.e., do not have the omitempty tag).
func getStructZeroValue(pass *analysis.Pass, structType *ast.StructType) string {
	value := "{"

	jsonTagInfo, ok := pass.ResultOf[extractjsontags.Analyzer].(extractjsontags.StructFieldTags)
	if !ok {
		panic("could not get struct field tags from pass result")
	}

	for _, field := range structType.Fields.List {
		fieldTagInfo := jsonTagInfo.FieldTags(field)

		if fieldTagInfo.OmitEmpty {
			// If the field is omitted, we can use a zero value.
			// For structs, if they aren't a pointer another error will be raised.
			continue
		}

		value += fmt.Sprintf("%q: %s, ", fieldTagInfo.Name, getTypedZeroValue(pass, field.Type))
	}

	value = strings.TrimSuffix(value, ", ")
	value += "}"

	return value
}

// getTypedValidationHint returns a string hint for the validation that should be applied to a given type.
// This is used to suggest which markers should be applied to the field to complete the validation.
func getTypedValidationHint(pass *analysis.Pass, expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return getIdentValidationHint(pass, t)
	case *ast.StructType:
		return "min properties/adding required fields"
	case *ast.ArrayType:
		return "min items"
	case *ast.MapType:
		return "min properties"
	default:
		return ""
	}
}

// getIdentValidationHint returns a string hint for the validation that should be applied to a given identifier.
func getIdentValidationHint(pass *analysis.Pass, ident *ast.Ident) string {
	switch {
	case isIntegerIdent(ident):
		return "minimum/maximum"
	case isStringIdent(ident):
		return "minimum length"
	case isBoolIdent(ident):
		return ""
	case isFloatIdent(ident):
		return "minimum/maximum"
	}

	typeSpec, ok := utils.LookupTypeSpec(pass, ident)
	if !ok {
		return ""
	}

	return getTypedValidationHint(pass, typeSpec.Type)
}

// isStructType checks if the given expression is a struct type.
func isStructType(pass *analysis.Pass, expr ast.Expr) bool {
	_, underlying := isStarExpr(expr)

	if _, ok := underlying.(*ast.StructType); ok {
		return true
	}

	// Where there's an ident, recurse to find the underlying type.
	if ident, ok := underlying.(*ast.Ident); ok {
		typeSpec, ok := utils.LookupTypeSpec(pass, ident)
		if !ok {
			return false
		}

		return isStructType(pass, typeSpec.Type)
	}

	return false
}
