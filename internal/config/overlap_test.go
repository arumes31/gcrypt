package config

import "testing"

func TestPairsOverlap(t *testing.T) {
	cases := []struct {
		name string
		a    SyncPair
		b    SyncPair
		want bool
	}{
		{
			name: "identical local dirs",
			a:    SyncPair{LocalDir: `C:\Data`, DriveFolderID: "x"},
			b:    SyncPair{LocalDir: `C:\Data`, DriveFolderID: "y"},
			want: true,
		},
		{
			name: "nested local dir",
			a:    SyncPair{LocalDir: `C:\Data`, DriveFolderID: "x"},
			b:    SyncPair{LocalDir: `C:\Data\Sub`, DriveFolderID: "y"},
			want: true,
		},
		{
			name: "same drive folder",
			a:    SyncPair{LocalDir: `C:\A`, DriveFolderID: "shared"},
			b:    SyncPair{LocalDir: `C:\B`, DriveFolderID: "shared"},
			want: true,
		},
		{
			name: "disjoint",
			a:    SyncPair{LocalDir: `C:\A`, DriveFolderID: "x"},
			b:    SyncPair{LocalDir: `C:\B`, DriveFolderID: "y"},
			want: false,
		},
		{
			name: "sibling prefix not nested",
			a:    SyncPair{LocalDir: `C:\Data`, DriveFolderID: "x"},
			b:    SyncPair{LocalDir: `C:\DataBackup`, DriveFolderID: "y"},
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := PairsOverlap(&c.a, &c.b); got != c.want {
				t.Fatalf("PairsOverlap = %v, want %v", got, c.want)
			}
		})
	}
}

func TestEffectiveDirectionMigratesForwardOnly(t *testing.T) {
	if d := (&SyncPair{}).EffectiveDirection(); d != SyncDirTwoWay {
		t.Fatalf("default direction = %q, want two-way", d)
	}
	if d := (&SyncPair{ForwardOnly: true}).EffectiveDirection(); d != SyncDirUploadOnly {
		t.Fatalf("ForwardOnly should map to upload-only, got %q", d)
	}
	if d := (&SyncPair{ForwardOnly: true, SyncDirection: SyncDirDownloadOnly}).EffectiveDirection(); d != SyncDirDownloadOnly {
		t.Fatalf("explicit SyncDirection should win over ForwardOnly, got %q", d)
	}
}
