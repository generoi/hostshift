package main

import ("bytes";"fmt";"golang.org/x/net/html")
func main() {
	for _, s := range []string{
		`<svg><title><a href="https://www.acmecorp.fi/x">y</a></title></svg>`,
		`<template><a href="https://www.acmecorp.fi/x">y</a></template>`,
		`<svg><style>.a{fill:url(https://www.acmecorp.fi/g)}</style></svg>`,
		`<math><mtext><a href="https://www.acmecorp.fi/x">y</a></mtext></math>`,
	} {
		z := html.NewTokenizer(bytes.NewReader([]byte(s)))
		fmt.Printf("INPUT: %s\n", s)
		var out bytes.Buffer
		for {
			tt := z.Next()
			if tt == html.ErrorToken { break }
			fmt.Printf("   %-16v %q\n", tt, z.Raw())
			out.Write(z.Raw())
		}
		fmt.Printf("   roundtrip identical: %v\n\n", out.String() == s)
	}
}
