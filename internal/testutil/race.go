package testutil

import "testing"

// SkipBudgetUnderRace пропускает однопоточный бюджетный тест (байты на диск,
// выделения) под -race: гонок в нём нет, инструментирование растягивает его
// на минуты и раздувает выделения. Без -race тест идёт в гейте.
func SkipBudgetUnderRace(t testing.TB) {
	t.Helper()
	if Race {
		t.Skip("-race: однопоточный бюджетный тест идёт в гейте без -race")
	}
}
