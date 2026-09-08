package crmevents

import (
	"strings"

	"github.com/sk1fy/amocrm-pro/internal/serviceapi"
)

// eventCategorySQL is built only from the closed product mapping. Dynamic
// custom_field_{id} and *_linked suffixes stay pattern matches, not a second taxonomy.
func eventCategorySQL(column string) string {
	if column != "e.event_type" {
		return "'other'"
	}
	var b strings.Builder
	b.WriteString("CASE")
	for _, category := range serviceapi.KnownCategories() {
		var types []string
		for _, kind := range serviceapi.ExactCategoryTypes()[category] {
			if serviceapi.SafeSQLToken(kind) {
				types = append(types, "'"+kind+"'")
			}
		}
		if len(types) == 0 {
			continue
		}
		b.WriteString(" WHEN ")
		b.WriteString(column)
		b.WriteString(" IN (")
		b.WriteString(strings.Join(types, ","))
		b.WriteString(") THEN '")
		b.WriteString(category)
		b.WriteString("'")
	}
	b.WriteString(" WHEN ")
	b.WriteString(column)
	b.WriteString(" LIKE 'custom_field!_%!_value_changed' ESCAPE '!' THEN 'custom_fields'")
	b.WriteString(" WHEN ")
	b.WriteString(column)
	b.WriteString(" LIKE '%!_linked' ESCAPE '!' OR ")
	b.WriteString(column)
	b.WriteString(" LIKE '%!_unlinked' ESCAPE '!' THEN 'relations'")
	b.WriteString(" ELSE 'other' END")
	return b.String()
}
