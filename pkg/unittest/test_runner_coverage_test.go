package unittest_test

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	. "github.com/helm-unittest/helm-unittest/pkg/unittest"
	"github.com/helm-unittest/helm-unittest/pkg/unittest/printer"
	"github.com/stretchr/testify/assert"
)

func TestV3RunnerCoverageSummaryAndJsonReport(t *testing.T) {
	chart := `
apiVersion: v2
name: coverage
version: 0.1.0
`
	deploymentTemplate := `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Release.Name }}-deployment
`
	serviceTemplate := `
apiVersion: v1
kind: Service
metadata:
  name: {{ .Release.Name }}-service
`
	testSuite := `
suite: coverage suite
templates:
  - templates/deployment.yaml
tests:
  - it: should render deployment
    asserts:
      - isKind:
          of: Deployment
`

	tmp := t.TempDir()
	coverageOutputPath := filepath.Join(tmp, "coverage-report.json")

	fs := fstest.MapFS{
		"chart/Chart.yaml":                 {Data: []byte(chart)},
		"chart/templates/deployment.yaml":  {Data: []byte(deploymentTemplate)},
		"chart/templates/service.yaml":     {Data: []byte(serviceTemplate)},
		"chart/tests/deployment_test.yaml": {Data: []byte(testSuite)},
	}
	for filePath, file := range fs {
		err := os.MkdirAll(filepath.Dir(filepath.Join(tmp, filePath)), 0755)
		assert.NoError(t, err)
		err = os.WriteFile(filepath.Join(tmp, filePath), file.Data, 0644)
		assert.NoError(t, err)
	}

	buffer := new(bytes.Buffer)
	runner := TestRunner{
		Printer:            printer.NewPrinter(buffer, nil),
		TestFiles:          []string{"tests/*_test.yaml"},
		Coverage:           true,
		CoverageOutputFile: coverageOutputPath,
	}

	passed := runner.RunV3([]string{filepath.Join(tmp, "chart")})
	assert.True(t, passed, buffer.String())
	assert.Contains(t, buffer.String(), "Template Coverage: 1 of 2 templates covered (50.0%)")
	assert.Contains(t, buffer.String(), "- coverage/templates/service.yaml")

	reportBytes, err := os.ReadFile(coverageOutputPath)
	assert.NoError(t, err)

	report := map[string]any{}
	err = json.Unmarshal(reportBytes, &report)
	assert.NoError(t, err)
	assert.EqualValues(t, 2, report["totalTemplates"])
	assert.EqualValues(t, 1, report["coveredTemplates"])
	// Branch coverage totals should be present
	assert.Contains(t, report, "totalBranches")
	assert.Contains(t, report, "coveredBranchEstimate")
	assert.Contains(t, report, "branchCoveragePercent")
}

// TestV3RunnerBranchCoverage verifies that the tracker correctly distinguishes between
// test cases that render a template with different values (exercising different conditional
// branches) and reports a branch-coverage estimate in the console output and JSON report.
func TestV3RunnerBranchCoverage(t *testing.T) {
	chartYaml := `
apiVersion: v2
name: branchcov
version: 0.1.0
`
	// Template with a top-level conditional: 2 branches (if true, if false/empty)
	serviceTemplate := `{{- if .Values.service.enabled }}
apiVersion: v1
kind: Service
metadata:
  name: {{ .Release.Name }}-service
{{- end }}
`
	// Test 1: service enabled (true branch)
	testEnabled := `
suite: service enabled suite
templates:
  - templates/service.yaml
tests:
  - it: should render service when enabled
    set:
      service.enabled: true
    asserts:
      - isKind:
          of: Service
`
	// Test 2: service disabled (false branch – template renders empty)
	testDisabled := `
suite: service disabled suite
templates:
  - templates/service.yaml
tests:
  - it: should not render service when disabled
    set:
      service.enabled: false
    asserts:
      - hasDocuments:
          count: 0
`

	tmp := t.TempDir()
	coverageOutputPath := filepath.Join(tmp, "branch-coverage.json")

	files := map[string][]byte{
		"chart/Chart.yaml":                       []byte(chartYaml),
		"chart/templates/service.yaml":            []byte(serviceTemplate),
		"chart/tests/enabled_test.yaml":           []byte(testEnabled),
		"chart/tests/disabled_test.yaml":          []byte(testDisabled),
	}
	for relPath, data := range files {
		fullPath := filepath.Join(tmp, relPath)
		assert.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0755))
		assert.NoError(t, os.WriteFile(fullPath, data, 0644))
	}

	buffer := new(bytes.Buffer)
	runner := TestRunner{
		Printer:            printer.NewPrinter(buffer, nil),
		TestFiles:          []string{"tests/*_test.yaml"},
		Coverage:           true,
		CoverageOutputFile: coverageOutputPath,
	}

	passed := runner.RunV3([]string{filepath.Join(tmp, "chart")})
	assert.True(t, passed, buffer.String())

	// Template-level: 1 of 1 covered
	assert.Contains(t, buffer.String(), "Template Coverage: 1 of 1 templates covered (100.0%)")
	// Branch summary line should appear
	assert.Contains(t, buffer.String(), "Branch Coverage (est.):")

	reportBytes, err := os.ReadFile(coverageOutputPath)
	assert.NoError(t, err)

	var report map[string]any
	assert.NoError(t, json.Unmarshal(reportBytes, &report))

	assert.EqualValues(t, 1, report["totalTemplates"])
	assert.EqualValues(t, 1, report["coveredTemplates"])

	// The template has 1 {{if}} → 2 branch points. Two distinct renders (non-empty + empty)
	// should drive the estimate to 2/2 = 100%.
	assert.EqualValues(t, 2, report["totalBranches"])
	assert.EqualValues(t, 2, report["coveredBranchEstimate"])
	assert.InDelta(t, 100.0, report["branchCoveragePercent"], 0.01)

	// Per-file details
	reportFiles := report["files"].([]any)
	assert.Len(t, reportFiles, 1)
	fileEntry := reportFiles[0].(map[string]any)
	assert.Equal(t, "branchcov/templates/service.yaml", fileEntry["template"])
	assert.InDelta(t, 100.0, fileEntry["branchCoveragePercent"], 0.01)
}

// TestV3RunnerBranchCoveragePartial verifies that when only one branch of a conditional
// is exercised (e.g., service always enabled), the branch coverage estimate is less than 100%.
func TestV3RunnerBranchCoveragePartial(t *testing.T) {
	chartYaml := `
apiVersion: v2
name: partial
version: 0.1.0
`
	serviceTemplate := `{{- if .Values.service.enabled }}
apiVersion: v1
kind: Service
metadata:
  name: {{ .Release.Name }}-service
{{- end }}
`
	// Only tests the enabled=true path
	testEnabled := `
suite: service always enabled
templates:
  - templates/service.yaml
tests:
  - it: should render service
    set:
      service.enabled: true
    asserts:
      - isKind:
          of: Service
`

	tmp := t.TempDir()
	coverageOutputPath := filepath.Join(tmp, "partial-coverage.json")

	files := map[string][]byte{
		"chart/Chart.yaml":                  []byte(chartYaml),
		"chart/templates/service.yaml":       []byte(serviceTemplate),
		"chart/tests/service_test.yaml":      []byte(testEnabled),
	}
	for relPath, data := range files {
		fullPath := filepath.Join(tmp, relPath)
		assert.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0755))
		assert.NoError(t, os.WriteFile(fullPath, data, 0644))
	}

	buffer := new(bytes.Buffer)
	runner := TestRunner{
		Printer:            printer.NewPrinter(buffer, nil),
		TestFiles:          []string{"tests/*_test.yaml"},
		Coverage:           true,
		CoverageOutputFile: coverageOutputPath,
	}

	passed := runner.RunV3([]string{filepath.Join(tmp, "chart")})
	assert.True(t, passed, buffer.String())

	reportBytes, err := os.ReadFile(coverageOutputPath)
	assert.NoError(t, err)

	var report map[string]any
	assert.NoError(t, json.Unmarshal(reportBytes, &report))

	// Only the true branch was exercised → estimate = 1 of 2 = 50%
	assert.EqualValues(t, 2, report["totalBranches"])
	assert.EqualValues(t, 1, report["coveredBranchEstimate"])
	assert.InDelta(t, 50.0, report["branchCoveragePercent"], 0.01)

	// The "Low Branch Coverage" section should appear in the console output
	assert.Contains(t, buffer.String(), "Low Branch Coverage:")
}

// TestV3RunnerCoberturaReport verifies that a valid Cobertura XML coverage report is written
// when --coverage-cobertura-file is specified. It checks that the root attributes and per-class
// branch/line attributes match expected values.
func TestV3RunnerCoberturaReport(t *testing.T) {
chartYaml := `
apiVersion: v2
name: cobertcov
version: 0.1.0
`
// Template with one {{if}} block → 2 branches
serviceTemplate := `{{- if .Values.service.enabled }}
apiVersion: v1
kind: Service
metadata:
  name: {{ .Release.Name }}-service
{{- end }}
`
// Test 1: enabled=true  (non-empty render)
testEnabled := `
suite: service enabled
templates:
  - templates/service.yaml
tests:
  - it: service enabled
    set:
      service.enabled: true
    asserts:
      - isKind:
          of: Service
`
// Test 2: enabled=false (empty render – exercises the false branch)
testDisabled := `
suite: service disabled
templates:
  - templates/service.yaml
tests:
  - it: service disabled
    set:
      service.enabled: false
    asserts:
      - hasDocuments:
          count: 0
`

tmp := t.TempDir()
coberturaPath := filepath.Join(tmp, "coverage.xml")

files := map[string][]byte{
"chart/Chart.yaml":              []byte(chartYaml),
"chart/templates/service.yaml":  []byte(serviceTemplate),
"chart/tests/enabled_test.yaml":  []byte(testEnabled),
"chart/tests/disabled_test.yaml": []byte(testDisabled),
}
for relPath, data := range files {
fullPath := filepath.Join(tmp, relPath)
assert.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0755))
assert.NoError(t, os.WriteFile(fullPath, data, 0644))
}

buffer := new(bytes.Buffer)
runner := TestRunner{
Printer:               printer.NewPrinter(buffer, nil),
TestFiles:             []string{"tests/*_test.yaml"},
Coverage:              true,
CoverageCoberturaFile: coberturaPath,
}

passed := runner.RunV3([]string{filepath.Join(tmp, "chart")})
assert.True(t, passed, buffer.String())

xmlBytes, err := os.ReadFile(coberturaPath)
assert.NoError(t, err)
assert.NotEmpty(t, xmlBytes)

// Parse the generated XML into a generic structure for assertion.
type xmlCondition struct {
Coverage string `xml:"coverage,attr"`
}
type xmlLine struct {
Number            int            `xml:"number,attr"`
Hits              int            `xml:"hits,attr"`
Branch            bool           `xml:"branch,attr"`
ConditionCoverage string         `xml:"condition-coverage,attr"`
Conditions        []xmlCondition `xml:"conditions>condition"`
}
type xmlClass struct {
Name       string    `xml:"name,attr"`
LineRate   float64   `xml:"line-rate,attr"`
BranchRate float64   `xml:"branch-rate,attr"`
Lines      []xmlLine `xml:"lines>line"`
}
type xmlPackage struct {
Name    string     `xml:"name,attr"`
Classes []xmlClass `xml:"classes>class"`
}
type xmlCoverage struct {
XMLName         xml.Name     `xml:"coverage"`
LinesValid      int          `xml:"lines-valid,attr"`
LinesCovered    int          `xml:"lines-covered,attr"`
LineRate        float64      `xml:"line-rate,attr"`
BranchesValid   int          `xml:"branches-valid,attr"`
BranchesCovered int          `xml:"branches-covered,attr"`
BranchRate      float64      `xml:"branch-rate,attr"`
Packages        []xmlPackage `xml:"packages>package"`
}

var cov xmlCoverage
assert.NoError(t, xml.Unmarshal(xmlBytes, &cov))

// Top-level attributes
assert.Equal(t, 1, cov.LinesValid, "one template = one line")
assert.Equal(t, 1, cov.LinesCovered, "template was rendered non-empty")
assert.InDelta(t, 1.0, cov.LineRate, 0.001)
assert.Equal(t, 2, cov.BranchesValid, "{{if}} block → 2 branches")
assert.Equal(t, 2, cov.BranchesCovered, "both branches exercised")
assert.InDelta(t, 1.0, cov.BranchRate, 0.001)

// Package and class structure
assert.Len(t, cov.Packages, 1)
pkg := cov.Packages[0]
assert.Equal(t, "cobertcov/templates", pkg.Name)
assert.Len(t, pkg.Classes, 1)

cls := pkg.Classes[0]
assert.Equal(t, "service.yaml", cls.Name)
assert.InDelta(t, 1.0, cls.LineRate, 0.001)
assert.InDelta(t, 1.0, cls.BranchRate, 0.001)

// Line details
assert.Len(t, cls.Lines, 1)
line := cls.Lines[0]
assert.Equal(t, 1, line.Number)
assert.Equal(t, 1, line.Hits)     // one non-empty render
assert.True(t, line.Branch)        // has conditional branches
assert.Contains(t, line.ConditionCoverage, "100%")
}
