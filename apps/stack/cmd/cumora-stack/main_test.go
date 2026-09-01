// main 单测(#281 评审 P2):splitComma/splitCSV 的边界 —— 尾逗号、
// 空串、带空格元素是 flag 面最常见的输入形态。
package main

import (
	"reflect"
	"testing"
)

func TestSplitCommaEdges(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"a,b", []string{"a", "b"}},
		{"a,", []string{"a", ""}}, // 原样保留空段,清洗职责归 splitCSV
		{",b", []string{"", "b"}},
		{",,", []string{"", "", ""}},
		{"", []string{""}},
		{"solo", []string{"solo"}},
	}
	for _, c := range cases {
		if got := splitComma(c.in); !reflect.DeepEqual(got, c.want) {
			t.Fatalf("splitComma(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestSplitCSVCleans(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{" cumora-go , cumora-daemon ", []string{"cumora-go", "cumora-daemon"}},
		{"a,,b", []string{"a", "b"}},
		{" , ", nil},
		{"", nil},
	}
	for _, c := range cases {
		if got := splitCSV(c.in); !reflect.DeepEqual(got, c.want) {
			t.Fatalf("splitCSV(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
