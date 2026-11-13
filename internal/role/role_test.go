package role

import "testing"

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    Role
		wantErr bool
	}{
		{"", All, false}, // the documented legacy default
		{"admin", Admin, false},
		{"cms", CMS, false},
		{"all", All, false},
		{" CMS ", CMS, false},
		{"gateway", "", true},
	} {
		got, err := Parse(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("Parse(%q) accepted garbage: %q", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Fatalf("Parse(%q) = %q, %v want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestPlaneMapping(t *testing.T) {
	if Admin.Planes() != PlaneAdmin || CMS.Planes() != PlaneCMS {
		t.Fatal("single-plane roles mis-mapped")
	}
	if All.Planes() != PlanesAll {
		t.Fatal("legacy all must serve both planes")
	}
	if !(PlaneAdmin | PlaneCMS).Has(PlaneAdmin) || PlaneAdmin.Has(PlaneCMS) {
		t.Fatal("Has is not an intersection test")
	}
}
