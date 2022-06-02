package basic

import (
	"fmt"

	"github.com/hashicorp/enos/internal/flightplan"
	"github.com/hashicorp/enos/internal/ui/status"
	"github.com/hashicorp/enos/proto/hashicorp/enos/v1/pb"
)

// ShowScenarioOutput shows the scenario outputs
func (v *View) ShowScenarioOutput(res *pb.OutputScenariosResponse) error {
	for _, out := range res.GetResponses() {
		scenario := flightplan.NewScenario()
		scenario.FromRef(out.GetTerraformModule().GetScenarioRef())

		v.ui.Info(fmt.Sprintf("Scenario: %s", scenario.String()))
		v.writeOutputResponse(out.GetOutput())
	}

	v.WriteDiagnostics(res.GetDiagnostics())

	return status.OutputScenarios(res)
}
