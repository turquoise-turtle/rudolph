package request

// Shape counts what decoding a JSON body would allocate per element, without decoding it. Decoding a few bytes of
// body such as `{},` or `null,` into a slice of structs allocates a whole struct for each, hundreds of bytes, so a
// handler that knows how many items it can ever legitimately receive checks the counts first and refuses a body
// above them.
type Shape struct {
	// Values is the number of objects plus the number of array elements, anywhere in the document; an object that is
	// an array element counts twice.
	Values int
	// TopLevelItems is the number of elements of the arrays that are members of the root object, for example the
	// events of an upload.
	TopLevelItems int
}

// ShapeOf scans a JSON document once, in time linear in its length. Bytes inside strings are skipped, so brackets in
// values do not count. A document that is not valid JSON gives counts that mean nothing; the decoder refuses it
// afterwards.
func ShapeOf(data []byte) Shape {
	var shape Shape
	// inArray[d] is set while the container at depth d+1 is an array.
	var inArray []bool
	element := func() {
		depth := len(inArray)
		if depth == 0 || !inArray[depth-1] {
			return
		}
		shape.Values++
		if depth == 2 && !inArray[0] {
			shape.TopLevelItems++
		}
	}
	for i := 0; i < len(data); i++ {
		switch c := data[i]; {
		case c == '"':
			// A string decodes into nothing useful where a struct is expected, but encoding/json keeps going after a
			// type error, so the slice it fills still grows by a whole element for it.
			element()
			i = stringEnd(data, i) - 1
		case c == '{' || c == '[':
			element()
			if c == '{' {
				shape.Values++
			}
			inArray = append(inArray, c == '[')
		case c == '}' || c == ']':
			if len(inArray) > 0 {
				inArray = inArray[:len(inArray)-1]
			}
		case c == '-', c >= '0' && c <= '9', c == 't', c == 'f', c == 'n':
			element()
			i = scalarEnd(data, i) - 1
		}
	}
	return shape
}
