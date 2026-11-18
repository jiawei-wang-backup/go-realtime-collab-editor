package collab

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

type OperationKind string

const (
	OperationInsert OperationKind = "insert"
	OperationDelete OperationKind = "delete"
)

type Document struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Version uint64 `json:"version"`
}

type Operation struct {
	ID          string        `json:"id,omitempty"`
	UserID      string        `json:"user_id,omitempty"`
	BaseVersion uint64        `json:"base_version,omitempty"`
	Kind        OperationKind `json:"kind"`
	Position    int           `json:"position"`
	Text        string        `json:"text,omitempty"`
	Length      int           `json:"length,omitempty"`
}

type AppliedOperation struct {
	Operation
	Version uint64 `json:"version"`
}

func (op Operation) Validate(content string) error {
	runeCount := utf8.RuneCountInString(content)
	if op.Position < 0 || op.Position > runeCount {
		return fmt.Errorf("position %d is outside document", op.Position)
	}

	switch op.Kind {
	case OperationInsert:
		if op.Text == "" {
			return fmt.Errorf("insert text must not be empty")
		}
		if op.Length != 0 {
			return fmt.Errorf("insert length must be zero")
		}
	case OperationDelete:
		if op.Text != "" {
			return fmt.Errorf("delete text must be empty")
		}
		if op.Length < 0 || op.Length > runeCount-op.Position {
			return fmt.Errorf("delete range is outside document")
		}
	default:
		return fmt.Errorf("unsupported operation kind %q", op.Kind)
	}

	return nil
}

func Apply(content string, op Operation) (string, error) {
	if err := op.Validate(content); err != nil {
		return "", err
	}

	// 原有的 rune 转换会把每个无效字节替换为 U+FFFD。先分别处理两段输入，
	// 避免拼接处的无效字节组合成另一个字符。
	content = normalizeEditText(content)
	inserted := normalizeEditText(op.Text)
	if op.Kind == OperationDelete && op.Length == 0 {
		return content, nil
	}
	start := byteOffset(content, op.Position)
	end := start
	if op.Kind == OperationDelete {
		end += byteOffset(content[start:], op.Length)
	}

	// 一次分配结果，即使只保留很小的前缀或后缀也能释放旧正文。
	var result strings.Builder
	result.Grow(len(content) - (end - start) + len(inserted))
	result.WriteString(content[:start])
	result.WriteString(inserted)
	result.WriteString(content[end:])
	return result.String(), nil
}

func normalizeEditText(text string) string {
	if utf8.ValidString(text) {
		return text
	}
	return string([]rune(text))
}

// 调用前已校验字符位置，遍历到末尾时返回 EOF 对应的字节位置。
func byteOffset(text string, characters int) int {
	for offset := range text {
		if characters == 0 {
			return offset
		}
		characters--
	}
	return len(text)
}
