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
