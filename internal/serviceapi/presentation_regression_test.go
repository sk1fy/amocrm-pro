package serviceapi

import (
	"testing"
	"time"
)

func TestAutoBucketsUseActualBucketCount(t *testing.T) {
	for _, tc := range []struct {
		name     string
		from     int64
		duration int64
		want     string
	}{
		{"aligned 48 hours", time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC).Unix(), 48*3600 - 1, BucketHour},
		{"unaligned 48 hours", time.Date(2026, 9, 1, 12, 30, 0, 0, time.UTC).Unix(), 48*3600 - 1, BucketDay},
		{"inclusive endpoint", time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC).Unix(), 48 * 3600, BucketDay},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := Query{From: tc.from, To: tc.from + tc.duration, Timezone: "UTC", Buckets: BucketAuto}
			unit, err := ResolveBuckets(q)
			if err != nil || unit != tc.want {
				t.Fatalf("unit=%s err=%v", unit, err)
			}
			ranges, err := QueryTimeBuckets(q)
			if err != nil {
				t.Fatal(err)
			}
			if ranges[len(ranges)-1].EndAt != q.To {
				t.Fatal("last bucket truncated")
			}
			if tc.want == BucketDay {
				q.Buckets = BucketHour
				if _, err = ResolveBuckets(q); ErrorCode(err) != InvalidArgument {
					t.Fatalf("explicit hour lost bound: %v", err)
				}
			}
		})
	}
}

func TestEntityLabelRequiresMatchingKindTypeAndID(t *testing.T) {
	names := []CatalogName{
		{Kind: ObjectEntity, EntityType: "contacts", ID: 31, Name: "Contact"},
		{Kind: ObjectCustomField, EntityType: "leads", ID: 31, Name: "Field"},
		{Kind: ObjectEntity, EntityType: "leads", ID: 31, Name: "Lead"},
	}
	for _, kind := range []string{"lead", "leads"} {
		if got := EntityLabel(kind, 31, names); got != "Lead" {
			t.Fatalf("%s label=%q", kind, got)
		}
	}
	if got := EntityLabel("contact", 31, names); got != "Contact" {
		t.Fatalf("contact label=%q", got)
	}
	if got := EntityLabel("lead", 31, names[:2]); got == "Contact" || got == "Field" {
		t.Fatalf("unsafe fallback=%q", got)
	}
	if got := EntityLabel("lead", 31, []CatalogName{{Kind: ObjectEntity, ID: 31, Name: "Unknown type"}}); got == "Unknown type" {
		t.Fatal("untyped name used")
	}
}
