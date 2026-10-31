package race

import (
	_ "embed" // the car table, cars.csv
	"encoding/csv"
	"strconv"
	"strings"
	"sync"
)

// The car table (paddock#86, R4): cars.csv, embedded in the binary, is v1's
// src/those_who_run/data/cars.csv at v1.0.0, unchanged: GT7's car code, maker and
// model, from ddm999/gt7info (MIT-0; NOTICE). GT7's packet carries the code only.
// Cars GT7 added after the table was taken are missing from it.
//
//go:embed cars.csv
var carsCSV string

var (
	carsOnce sync.Once
	cars     map[int32]string
)

// carName is v1's cars.car_name: the car's maker and model, "car <code>" for a
// car missing from the table, and "" for no car (a code of 0 or less, v1's None).
func carName(code int32) string {
	if code <= 0 {
		return ""
	}
	carsOnce.Do(func() { cars = parseCars(carsCSV) })
	if name, ok := cars[code]; ok {
		return name
	}
	return "car " + strconv.Itoa(int(code))
}

// parseCars reads the table as v1's load_cars does: lines starting with # are
// comments, then a header, then id, maker and name. It panics on a row it can't
// read: the table is embedded, so that is a build's mistake.
func parseCars(text string) map[int32]string {
	var lines []string
	for line := range strings.Lines(text) {
		if strings.TrimSpace(line) != "" && !strings.HasPrefix(line, "#") {
			lines = append(lines, line)
		}
	}
	rows, err := csv.NewReader(strings.NewReader(strings.Join(lines, ""))).ReadAll()
	if err != nil || len(rows) == 0 || strings.Join(rows[0], ",") != "id,maker,name" {
		panic("race: the embedded cars.csv does not read")
	}
	out := make(map[int32]string, len(rows)-1)
	for _, r := range rows[1:] {
		id, err := strconv.ParseInt(r[0], 10, 32)
		if err != nil {
			panic("race: the embedded cars.csv: " + err.Error())
		}
		out[int32(id)] = fullName(r[1], r[2])
	}
	return out
}

// fullName is v1's Car.full_name: maker and model, e.g. "Honda Civic Si Group A
// version '87", or the model alone when it already starts with the maker.
func fullName(maker, name string) string {
	if maker == "" || strings.HasPrefix(strings.ToLower(name), strings.ToLower(maker)) {
		return name
	}
	return maker + " " + name
}
