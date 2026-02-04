package main

import (
"fmt"
"reflect"
"strings"
"github.com/hashicorp/serf/serf"
)

func main() {
c := serf.DefaultConfig()
v := reflect.ValueOf(c).Elem()
t := v.Type()

for i := 0; i < t.NumField(); i++ {
field := t.Field(i)
if strings.Contains(strings.ToLower(field.Name), "tag") || strings.Contains(strings.ToLower(field.Name), "size") {
fmt.Printf("%s: %v\n", field.Name, v.Field(i).Interface())
}
}
}
