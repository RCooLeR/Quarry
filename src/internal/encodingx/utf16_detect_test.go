package encodingx

import (
	"testing"
)

func TestDetectBOMlessUTF16AcrossScripts(t *testing.T) {
	texts := map[string]string{
		"Cyrillic": "Привет мир\nВторая строка\n",
		"Greek":    "Καλημέρα κόσμε\nδεύτερη γραμμή\n",
		"CJK":      "你好 世界\n第二行 数据\n",
		"emoji":    "status 😀🙂 complete\nnext 🚀 row\n",
		"mixed":    "Latin Ελληνικά Кириллица 中文 😀\n",
	}
	for script, text := range texts {
		for _, encoding := range []string{"UTF-16LE", "UTF-16BE"} {
			t.Run(script+"/"+encoding, func(t *testing.T) {
				data, err := EncodeString(encoding, text)
				if err != nil {
					t.Fatal(err)
				}
				info := DetectSample(data)
				if info.Name != encoding || info.RequiresConfirmation {
					t.Fatalf("DetectSample(%s)=%+v, want %s", script, info, encoding)
				}

				truncated := data[:len(data)-1]
				prefix := DetectPrefixSample(truncated)
				if prefix.Name != encoding || prefix.RequiresConfirmation {
					t.Fatalf("DetectPrefixSample(%s)=%+v, want %s", script, prefix, encoding)
				}
				complete := DetectSample(truncated)
				if complete.Name == encoding && !complete.RequiresConfirmation {
					t.Fatalf("complete detector accepted odd-length %s input: %+v", encoding, complete)
				}
			})
		}
	}
}

func TestDetectBOMlessUTF16PureCJKNeverGuessesWrongEndian(t *testing.T) {
	for _, encoding := range []string{"UTF-16LE", "UTF-16BE"} {
		data, err := EncodeString(encoding, "你好世界数据库文件")
		if err != nil {
			t.Fatal(err)
		}
		info := DetectSample(data)
		if info.Name != encoding && !info.RequiresConfirmation {
			t.Fatalf("pure CJK %s detected unsafely as %+v", encoding, info)
		}
	}
}

func TestDetectBOMlessUTF16AmbiguityRequiresConfirmation(t *testing.T) {
	// Both byte orders decode these pairs into assigned CJK letters. There is no
	// trustworthy endian evidence, so guessing would be data-corrupting.
	data := []byte{
		0x4E, 0x9F, 0x4F, 0x9E, 0x50, 0x9D, 0x51, 0x9C,
		0x52, 0x9B, 0x53, 0x9A, 0x54, 0x99, 0x55, 0x98,
	}
	info := DetectSample(data)
	if !info.RequiresConfirmation || info.Name != ambiguousUTF16Name {
		t.Fatalf("ambiguous UTF-16=%+v, want explicit confirmation", info)
	}
}

func TestDetectBOMlessUTF16RejectsBinaryControls(t *testing.T) {
	data := []byte{
		0x01, 0x00, 0x02, 0x00, 0x03, 0x00, 0x04, 0x00,
		0x05, 0x00, 0x06, 0x00, 0x07, 0x00, 0x08, 0x00,
	}
	info := DetectSample(data)
	if info.Name == "UTF-16LE" || info.Name == "UTF-16BE" || info.RequiresConfirmation {
		t.Fatalf("binary controls classified as UTF-16 text: %+v", info)
	}
}
