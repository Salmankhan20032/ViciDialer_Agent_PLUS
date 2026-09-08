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

	lastNames = []string{
		"Miller",
		"Davis",
		"Wilson",
		"Taylor",
		"Anderson",
		"Thomas",
		"Jackson",
		"White",
		"Harris",
		"Martin",
		"Thompson",
		"Robinson",
		"Clark",
		"Lewis",
		"Walker",
		"Hall",
		"Allen",
		"Young",
		"King",
		"Wright",
		"Scott",
		"Adams",
		"Baker",
		"Carter",
		"Mitchell",
	}
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

// AgentPersona holds complete agent identity details.
type AgentPersona struct {
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	FullName  string `json:"full_name"`
	Gender    string `json:"gender"`
	Company   string `json:"company"`
}

// PickAgentPersona selects a realistic American agent persona matching voice gender.
func PickAgentPersona(voice string) AgentPersona {
	v := strings.ToLower(strings.TrimSpace(voice))
	var first string
	var gender string
	switch v {
	case "aoede", "kore":
		first = femaleNames[rand.Intn(len(femaleNames))]
		gender = "female"
	case "puck", "charon", "fenrir":
		first = maleNames[rand.Intn(len(maleNames))]
		gender = "male"
	default:
		first = femaleNames[rand.Intn(len(femaleNames))]
		gender = "female"
	}

	last := lastNames[rand.Intn(len(lastNames))]
	return AgentPersona{
		FirstName: first,
		LastName:  last,
		FullName:  first + " " + last,
		Gender:    gender,
		Company:   "Senior Benefit Services",
	}
}

// PickAgentName keeps backwards compatibility.
func PickAgentName(voice string) (string, string) {
	p := PickAgentPersona(voice)
	return p.FirstName, p.Gender
}
