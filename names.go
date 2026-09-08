package main

import (
	"math/rand"
	"strings"
	"time"
)

var (
	femaleNames = []string{
		"Sarah",
		"Emily",
		"Jessica",
		"Ashley",
		"Amanda",
		"Rachel",
		"Hannah",
		"Megan",
		"Samantha",
		"Lauren",
		"Emma",
		"Olivia",
		"Chloe",
		"Grace",
	}

	maleNames = []string{
		"Michael",
		"David",
		"James",
		"John",
		"Robert",
		"Chris",
		"Brian",
		"Alex",
		"Daniel",
		"Matthew",
		"Brandon",
		"Jason",
		"Ryan",
		"Kevin",
	}
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

// PickAgentName selects a realistic American agent name matching the gender of the selected voice.
func PickAgentName(voice string) (string, string) {
	v := strings.ToLower(strings.TrimSpace(voice))
	switch v {
	case "aoede", "kore":
		return femaleNames[rand.Intn(len(femaleNames))], "female"
	case "puck", "charon", "fenrir":
		return maleNames[rand.Intn(len(maleNames))], "male"
	default:
		// Default to female for Aoede
		return femaleNames[rand.Intn(len(femaleNames))], "female"
	}
}
