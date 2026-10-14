package controller

import (
	"testing"

	controllertests "github.com/QuantumNous/new-api/tests/controller"
)

func TestInventorySupplyLimits(t *testing.T) {
	controllertests.VerifyInventorySupplyLimits(t, buildModelInventory)
}
