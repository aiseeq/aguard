package iowatch

import "testing"

func TestParseIOStatMax(t *testing.T) {
	tests := []struct {
		name        string
		data        string
		read, write int64
	}{
		{
			name:  "dm mirrors disk",
			data:  "259:0 rbytes=100 wbytes=200\n252:0 rbytes=100 wbytes=200\n",
			read:  100,
			write: 200,
		},
		{
			name:  "different counters use independent maxima",
			data:  "259:0 rbytes=900 wbytes=200\n252:0 rbytes=100 wbytes=800\n",
			read:  900,
			write: 800,
		},
		{
			name: "empty io.stat",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			read, write, err := ParseIOStatMax([]byte(tt.data))
			if err != nil {
				t.Fatal(err)
			}
			if read != tt.read || write != tt.write {
				t.Fatalf("got read=%d write=%d, want read=%d write=%d", read, write, tt.read, tt.write)
			}
		})
	}
}

func TestParseIOStatVRateKnownAndMissing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		data  string
		want  float64
		known bool
	}{
		{name: "known after other device", data: "252:0 cost.vrate=7.5\n259:0 rbytes=1 cost.vrate=29.10 wbytes=2\n", want: 29.1, known: true},
		{name: "missing field", data: "259:0 rbytes=1 wbytes=2\n", known: false},
		{name: "missing device", data: "252:0 cost.vrate=7.5\n", known: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, known, err := parseIOStatVRate([]byte(tc.data), "259:0")
			if err != nil || known != tc.known || (known && got != tc.want) {
				t.Fatalf("got %v known=%v err=%v; want %v known=%v", got, known, err, tc.want, tc.known)
			}
		})
	}
}
