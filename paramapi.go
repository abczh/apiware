// Copyright 2016 HenryLee. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package apiware

import (
	"errors"
	"fmt"
	"io/ioutil"
	"mime/multipart"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/valyala/fasthttp"
)

const (
	TAG_PARAM        = "param"  //request param tag name
	TAG_REGEXP       = "regexp" //regexp validate tag name(optio)
	TAG_ERR          = "err"    //customize the prompt for validation error(optio)
	TAG_IGNORE_PARAM = "-"      //ignore request param tag value

	MB                 = 1 << 20 // 1MB
	defaultMaxMemory   = 32 * MB // 32 MB
	defaultMaxMemoryMB = 32
)

type (
	// ParamsAPI defines a parameter model for an web api.
	ParamsAPI struct {
		name   string
		params []*Param
		//used to create a new struct (non-pointer)
		structType reflect.Type
		//the raw struct pointer
		rawStructPointer interface{}
		// create param name from struct field name
		paramNameFunc ParamNameFunc
		// decode params from request body
		bodyDecodeFunc BodyDecodeFunc
		//when request Content-Type is multipart/form-data, the max memory for body.
		maxMemory int64
	}

	// Schema is a collection of ParamsAPI
	Schema struct {
		lib map[string]*ParamsAPI
		sync.RWMutex
	}

	// Create param name from struct param name
	ParamNameFunc func(fieldName string) (paramName string)

	// Decode params from request body
	BodyDecodeFunc func(dest reflect.Value, body []byte) error
)

var (
	defaultSchema = &Schema{
		lib: map[string]*ParamsAPI{},
	}
)

// Parse and store the struct object, requires a struct pointer,
// if `paramNameFunc` is nil, `paramNameFunc=toSnake`,
// if `bodyDecodeFunc` is nil, `bodyDecodeFunc=bodyJONS`,
func NewParamsAPI(
	structPointer interface{},
	paramNameFunc ParamNameFunc,
	bodyDecodeFunc BodyDecodeFunc,
) (
	*ParamsAPI,
	error,
) {
	name := reflect.TypeOf(structPointer).String()
	v := reflect.ValueOf(structPointer)
	if v.Kind() != reflect.Ptr {
		return nil, NewError(name, "*", "the binding object must be a struct pointer")
	}
	v = reflect.Indirect(v)
	if v.Kind() != reflect.Struct {
		return nil, NewError(name, "*", "the binding object must be a struct pointer")
	}
	var m = &ParamsAPI{
		name:             name,
		params:           []*Param{},
		structType:       v.Type(),
		rawStructPointer: structPointer,
	}
	if paramNameFunc != nil {
		m.paramNameFunc = paramNameFunc
	} else {
		m.paramNameFunc = toSnake
	}
	if bodyDecodeFunc != nil {
		m.bodyDecodeFunc = bodyDecodeFunc
	} else {
		m.bodyDecodeFunc = bodyJONS
	}
	err := m.addFields([]int{}, m.structType, v)
	if err != nil {
		return nil, err
	}
	defaultSchema.set(m)
	return m, nil
}

// `Register` is similar to a `NewParamsAPI`, but only return error.
// Parse and store the struct object, requires a struct pointer,
// if `paramNameFunc` is nil, `paramNameFunc=toSnake`,
// if `bodyDecodeFunc` is nil, `bodyDecodeFunc=bodyJONS`,
func Register(
	structPointer interface{},
	paramNameFunc ParamNameFunc,
	bodyDecodeFunc BodyDecodeFunc,
) error {
	_, err := NewParamsAPI(structPointer, paramNameFunc, bodyDecodeFunc)
	return err
}

func (m *ParamsAPI) addFields(parentIndexPath []int, t reflect.Type, v reflect.Value) error {
	var err error
	var maxMemoryMB int64
	var hasFormData, hasBody bool
	var deep = len(parentIndexPath) + 1
	for i := 0; i < t.NumField(); i++ {
		indexPath := make([]int, deep)
		copy(indexPath, parentIndexPath)
		indexPath[deep-1] = i

		var field = t.Field(i)
		tag, ok := field.Tag.Lookup(TAG_PARAM)
		if !ok {
			if field.Anonymous && field.Type.Kind() == reflect.Struct {
				if err = m.addFields(indexPath, field.Type, v.Field(i)); err != nil {
					return err
				}
			}
			continue
		}

		if tag == TAG_IGNORE_PARAM {
			continue
		}

		if field.Type.Kind() == reflect.Ptr {
			return NewError(t.String(), field.Name, "field can not be a pointer")
		}

		var parsedTags = parseTags(tag)
		var paramPosition = parsedTags["in"]
		var paramTypeString = field.Type.String()

		switch paramTypeString {
		case fileTypeString:
			if paramPosition != "formData" {
				return NewError(t.String(), field.Name, "when field type is `"+paramTypeString+"`, tag `in` value must be `formData`")
			}
		case cookieTypeString, fasthttpCookieTypeString:
			if paramPosition != "cookie" {
				return NewError(t.String(), field.Name, "when field type is `"+paramTypeString+"`, tag `in` value must be `cookie`")
			}
		}

		switch paramPosition {
		case "formData":
			if hasBody {
				return NewError(t.String(), field.Name, "tags of `in(formData)` and `in(body)` can not exist at the same time")
			}
			hasFormData = true
		case "body":
			if hasFormData {
				return NewError(t.String(), field.Name, "tags of `in(formData)` and `in(body)` can not exist at the same time")
			}
			if hasBody {
				return NewError(t.String(), field.Name, "there should not be more than one tag `in(body)`")
			}
			hasBody = true
		case "path":
			parsedTags["required"] = "required"
		case "cookie":
			switch paramTypeString {
			case cookieTypeString, fasthttpCookieTypeString, stringTypeString, bytesTypeString, bytes2TypeString:
			default:
				return NewError(t.String(), field.Name, "invalid field type for `in(cookie)`, refer to the following: `http.Cookie`, `fasthttp.Cookie`, `string`, `[]byte` or `[]uint8`")
			}
		default:
			if !TagInValues[paramPosition] {
				return NewError(t.String(), field.Name, "invalid tag `in` value, refer to the following: `path`, `query`, `formData`, `body`, `header` or `cookie`")
			}
		}
		if _, ok := parsedTags["len"]; ok && paramTypeString != "string" && paramTypeString != "[]string" {
			return NewError(t.String(), field.Name, "invalid `len` tag for non-string field")
		}
		if _, ok := parsedTags["range"]; ok {
			switch paramTypeString {
			case "int", "int8", "int16", "int32", "int64", "uint", "uint8", "uint16", "uint32", "uint64", "float32", "float64":
			case "[]int", "[]int8", "[]int16", "[]int32", "[]int64", "[]uint", "[]uint8", "[]uint16", "[]uint32", "[]uint64", "[]float32", "[]float64":
			default:
				return NewError(t.String(), field.Name, "invalid `range` tag for non-number field")
			}
		}
		if a, ok := field.Tag.Lookup(TAG_REGEXP); ok {
			if paramTypeString != "string" && paramTypeString != "[]string" {
				return NewError(t.String(), field.Name, "invalid `"+TAG_REGEXP+"` tag for non-string field")
			}
			parsedTags[TAG_REGEXP] = a
		}
		if a, ok := parsedTags["maxmb"]; ok {
			i, err := strconv.ParseInt(a, 10, 64)
			if err != nil {
				return NewError(t.String(), field.Name, "invalid `maxmb` tag, it must be positive integer")
			}
			if i > maxMemoryMB {
				maxMemoryMB = i
			}
		}

		if errStr, ok := field.Tag.Lookup(TAG_ERR); ok {
			parsedTags[TAG_ERR] = errStr
		}

		// fmt.Printf("%#v\n", parsedTags)

		fd := &Param{
			indexPath: indexPath,
			tags:      parsedTags,
			rawTag:    field.Tag,
			rawValue:  v.Field(i),
		}

		if fd.name, ok = parsedTags["name"]; !ok {
			fd.name = m.paramNameFunc(field.Name)
		}

		fd.isFile = paramTypeString == fileTypeString
		_, fd.isRequired = parsedTags["required"]

		// err = fd.validate(v)
		// if err != nil {
		// 	return NewError(t.String(), field.Name, "the initial value failed validation:"+err.Error())
		// }

		m.params = append(m.params, fd)
	}
	if maxMemoryMB > 0 {
		m.maxMemory = maxMemoryMB * MB
	} else {
		m.maxMemory = defaultMaxMemory
	}
	return nil
}

func parseTags(s string) map[string]string {
	c := strings.Split(s, ",")
	m := make(map[string]string)
	for _, v := range c {
		c2 := strings.Split(v, "(")
		if len(c2) == 2 && len(c2[1]) > 1 {
			m[c2[0]] = c2[1][:len(c2[1])-1]
		} else {
			m[v] = ""
		}
	}
	return m
}

// get the `*ParamsAPI` object according to the type name
func GetParamsAPI(paramsAPIName string) (*ParamsAPI, error) {
	m, ok := defaultSchema.get(paramsAPIName)
	if !ok {
		return nil, errors.New("struct `" + paramsAPIName + "` is not registered")
	}
	return m, nil
}

// cache `*ParamsAPI`
func SetParamsAPI(m *ParamsAPI) {
	defaultSchema.set(m)
}

func (schema *Schema) get(paramsAPIName string) (*ParamsAPI, bool) {
	schema.RLock()
	defer schema.RUnlock()
	m, ok := schema.lib[paramsAPIName]
	return m, ok
}

func (schema *Schema) set(m *ParamsAPI) {
	schema.Lock()
	schema.lib[m.name] = m
	defer schema.Unlock()
}

// Get the name
func (paramsAPI *ParamsAPI) Name() string {
	return paramsAPI.name
}

// Get the parameter information
func (paramsAPI *ParamsAPI) Params() []*Param {
	return paramsAPI.params
}

// The number of parameters to be bound
func (paramsAPI *ParamsAPI) Number() int {
	return len(paramsAPI.params)
}

// return the ParamsAPI's original value
func (paramsAPI *ParamsAPI) Raw() interface{} {
	return paramsAPI.rawStructPointer
}

// get maxMemory
// when request Content-Type is multipart/form-data, the max memory for body.
func (paramsAPI *ParamsAPI) MaxMemory() int64 {
	return paramsAPI.maxMemory
}

// set maxMemory for the request which Content-Type is multipart/form-data.
func (paramsAPI *ParamsAPI) SetMaxMemory(maxMemory int64) {
	paramsAPI.maxMemory = maxMemory
}

// Creates a new struct pointer and the field's values  for its receive parameterste it.
func (paramsAPI *ParamsAPI) NewReceiver() (interface{}, []reflect.Value) {
	object := reflect.New(paramsAPI.structType)
	return object.Interface(), paramsAPI.fieldsForBinding(object.Elem())
}

func (paramsAPI *ParamsAPI) fieldsForBinding(structElem reflect.Value) []reflect.Value {
	count := len(paramsAPI.params)
	fields := make([]reflect.Value, count)
	for i := 0; i < count; i++ {
		value := structElem
		param := paramsAPI.params[i]
		for _, index := range param.indexPath {
			value = value.Field(index)
		}
		fields[i] = value
	}
	return fields
}

// Bind the net/http request params to a new struct and validate it.
func BindByName(
	paramsAPIName string,
	req *http.Request,
	pathParams KV,
) (
	interface{},
	error,
) {
	paramsAPI, err := GetParamsAPI(paramsAPIName)
	if err != nil {
		return nil, err
	}
	return paramsAPI.BindNew(req, pathParams)
}

// Bind the net/http request params to the `structPointer` param and validate it.
// note: structPointer must be struct pointer.
func Bind(
	structPointer interface{},
	req *http.Request,
	pathParams KV,
) error {
	paramsAPI, err := GetParamsAPI(reflect.TypeOf(structPointer).String())
	if err != nil {
		return err
	}
	return paramsAPI.BindAt(structPointer, req, pathParams)
}

// Bind the net/http request params to a struct pointer and validate it.
// note: structPointer must be struct pointer.
func (paramsAPI *ParamsAPI) BindAt(
	structPointer interface{},
	req *http.Request,
	pathParams KV,
) error {
	name := reflect.TypeOf(structPointer).String()
	if name != paramsAPI.name {
		return errors.New("the structPointer's type `" + name + "` does not match type `" + paramsAPI.name + "`")
	}
	return paramsAPI.BindFields(
		paramsAPI.fieldsForBinding(reflect.ValueOf(structPointer).Elem()),
		req,
		pathParams,
	)
}

// Bind the net/http request params to a struct pointer and validate it.
func (paramsAPI *ParamsAPI) BindNew(
	req *http.Request,
	pathParams KV,
) (
	interface{},
	error,
) {
	structPrinter, fields := paramsAPI.NewReceiver()
	err := paramsAPI.BindFields(fields, req, pathParams)
	return structPrinter, err
}

// Bind the net/http request params to the original struct pointer and validate it.
func (paramsAPI *ParamsAPI) RawBind(
	req *http.Request,
	pathParams KV,
) (
	interface{},
	error,
) {
	var fields []reflect.Value
	for _, param := range paramsAPI.params {
		fields = append(fields, param.rawValue)
	}
	err := paramsAPI.BindFields(fields, req, pathParams)
	return paramsAPI.rawStructPointer, err
}

// Bind the net/http request params to a struct and validate it.
// Must ensure that the param `fields` matches `paramsAPI.params`.
func (paramsAPI *ParamsAPI) BindFields(
	fields []reflect.Value,
	req *http.Request,
	pathParams KV,
) (
	err error,
) {
	if pathParams == nil {
		pathParams = Map(map[string]string{})
	}
	if req.Form == nil {
		req.ParseMultipartForm(paramsAPI.maxMemory)
	}
	var queryValues url.Values
	defer func() {
		if p := recover(); p != nil {
			err = NewError(paramsAPI.name, "?", fmt.Sprint(p))
		}
	}()

	for i, param := range paramsAPI.params {
		value := fields[i]
		switch param.In() {
		case "path":
			paramValue, ok := pathParams.Get(param.name)
			if !ok {
				return NewError(paramsAPI.name, param.name, "missing path param")
			}
			// fmt.Printf("paramName:%s\nvalue:%#v\n\n", param.name, paramValue)
			if err = convertAssign(value, []string{paramValue}); err != nil {
				return NewError(paramsAPI.name, param.name, err.Error())
			}

		case "query":
			if queryValues == nil {
				queryValues, err = url.ParseQuery(req.URL.RawQuery)
				if err != nil {
					queryValues = make(url.Values)
				}
			}
			paramValues, ok := queryValues[param.name]
			if ok {
				if err = convertAssign(value, paramValues); err != nil {
					return NewError(paramsAPI.name, param.name, err.Error())
				}
			} else if param.IsRequired() {
				return NewError(paramsAPI.name, param.name, "missing query param")
			}

		case "formData":
			// Can not exist with `body` param at the same time
			if param.IsFile() {
				if req.MultipartForm != nil {
					fhs := req.MultipartForm.File[param.name]
					if len(fhs) == 0 {
						if param.IsRequired() {
							return NewError(paramsAPI.name, param.name, "missing formData param")
						}
						continue
					}
					value.Set(reflect.ValueOf(fhs[0]).Elem())
				} else if param.IsRequired() {
					return NewError(paramsAPI.name, param.name, "missing formData param")
				}
				continue
			}

			paramValues, ok := req.PostForm[param.name]
			if ok {
				if err = convertAssign(value, paramValues); err != nil {
					return NewError(paramsAPI.name, param.name, err.Error())
				}
			} else if param.IsRequired() {
				return NewError(paramsAPI.name, param.name, "missing formData param")
			}

		case "body":
			// Theoretically there should be at most one `body` param, and can not exist with `formData` at the same time
			var body []byte
			body, err = ioutil.ReadAll(req.Body)
			req.Body.Close()
			if err == nil {
				if err = paramsAPI.bodyDecodeFunc(value, body); err != nil {
					return NewError(paramsAPI.name, param.name, err.Error())
				}
			} else if param.IsRequired() {
				return NewError(paramsAPI.name, param.name, "missing body param")
			}

		case "header":
			paramValues, ok := req.Header[param.name]
			if ok {
				if err = convertAssign(value, paramValues); err != nil {
					return NewError(paramsAPI.name, param.name, err.Error())
				}
			} else if param.IsRequired() {
				return NewError(paramsAPI.name, param.name, "missing header param")
			}

		case "cookie":
			c, _ := req.Cookie(param.name)
			if c != nil {
				switch value.Type().String() {
				case cookieTypeString:
					value.Set(reflect.ValueOf(c).Elem())

				case stringTypeString:
					value.Set(reflect.ValueOf(c.String()))

				case bytesTypeString, bytes2TypeString:
					value.Set(reflect.ValueOf([]byte(c.String())))

				default:
					return NewError(paramsAPI.name, param.name, "invalid cookie param type, it must be `http.Cookie`, `string` or `[]byte`")
				}
			} else if param.IsRequired() {
				return NewError(paramsAPI.name, param.name, "missing cookie param")
			}
		}
		if err = param.validate(value); err != nil {
			return err
		}
	}
	return
}

// Bind the fasthttp request params to a new struct pointer and validate it.
func FasthttpBindByName(
	paramsAPIName string,
	reqCtx *fasthttp.RequestCtx,
	pathParams KV,
) (
	interface{},
	error,
) {
	paramsAPI, err := GetParamsAPI(paramsAPIName)
	if err != nil {
		return nil, err
	}
	return paramsAPI.FasthttpBindNew(reqCtx, pathParams)
}

// Bind the fasthttp request params to the `structPointer` param and validate it.
// note: structPointer must be struct pointer.
func FasthttpBind(
	structPointer interface{},
	reqCtx *fasthttp.RequestCtx,
	pathParams KV,
) error {
	paramsAPI, err := GetParamsAPI(reflect.TypeOf(structPointer).String())
	if err != nil {
		return err
	}
	return paramsAPI.FasthttpBindAt(structPointer, reqCtx, pathParams)
}

// Bind the fasthttp request params to a struct pointer and validate it.
// note: structPointer must be struct pointer.
func (paramsAPI *ParamsAPI) FasthttpBindAt(
	structPointer interface{},
	reqCtx *fasthttp.RequestCtx,
	pathParams KV,
) error {
	name := reflect.TypeOf(structPointer).String()
	if name != paramsAPI.name {
		return errors.New("the structPointer's type `" + name + "` does not match type `" + paramsAPI.name + "`")
	}
	return paramsAPI.FasthttpBindFields(
		paramsAPI.fieldsForBinding(reflect.ValueOf(structPointer).Elem()),
		reqCtx,
		pathParams,
	)
}

// Bind the fasthttp request params to a struct pointer and validate it.
func (paramsAPI *ParamsAPI) FasthttpBindNew(
	reqCtx *fasthttp.RequestCtx,
	pathParams KV,
) (
	interface{},
	error,
) {
	structPointer, fields := paramsAPI.NewReceiver()
	return structPointer, paramsAPI.FasthttpBindFields(fields, reqCtx, pathParams)
}

// Bind the fasthttp request params to the original struct pointer and validate it.
func (paramsAPI *ParamsAPI) FasthttpRawBind(
	reqCtx *fasthttp.RequestCtx,
	pathParams KV,
) (
	interface{},
	error,
) {
	var fields []reflect.Value
	for _, param := range paramsAPI.params {
		fields = append(fields, param.rawValue)
	}
	err := paramsAPI.FasthttpBindFields(fields, reqCtx, pathParams)
	return paramsAPI.rawStructPointer, err
}

// Bind the fasthttp request params to the struct and validate.
// Must ensure that the param `fields` matches `paramsAPI.params`.
func (paramsAPI *ParamsAPI) FasthttpBindFields(
	fields []reflect.Value,
	reqCtx *fasthttp.RequestCtx,
	pathParams KV,
) (
	err error,
) {
	if pathParams == nil {
		pathParams = Map(map[string]string{})
	}

	defer func() {
		if p := recover(); p != nil {
			err = NewError(paramsAPI.name, "?", fmt.Sprint(p))
		}
	}()

	var formValues = fasthttpFormValues(reqCtx)
	for i, param := range paramsAPI.params {
		value := fields[i]
		switch param.In() {
		case "path":
			paramValue, ok := pathParams.Get(param.name)
			if !ok {
				return NewError(paramsAPI.name, param.name, "missing path param")
			}
			// fmt.Printf("paramName:%s\nvalue:%#v\n\n", param.name, paramValue)
			if err = convertAssign(value, []string{paramValue}); err != nil {
				return NewError(paramsAPI.name, param.name, err.Error())
			}

		case "query":
			paramValuesBytes := reqCtx.QueryArgs().PeekMulti(param.name)
			if len(paramValuesBytes) > 0 {
				var paramValues = make([]string, len(paramValuesBytes))
				for i, b := range paramValuesBytes {
					paramValues[i] = string(b)
				}
				if err = convertAssign(value, paramValues); err != nil {
					return NewError(paramsAPI.name, param.name, err.Error())
				}
			} else if len(paramValuesBytes) == 0 && param.IsRequired() {
				return NewError(paramsAPI.name, param.name, "missing query param")
			}

		case "formData":
			// Can not exist with `body` param at the same time
			if param.IsFile() {
				var fh *multipart.FileHeader
				if fh, err = reqCtx.FormFile(param.name); err != nil {
					if param.IsRequired() {
						return NewError(paramsAPI.name, param.name, "missing formData param")
					}
					continue
				}
				value.Set(reflect.ValueOf(fh).Elem())
				continue
			}

			paramValues, ok := formValues[param.name]
			if ok {
				if err = convertAssign(value, paramValues); err != nil {
					return NewError(paramsAPI.name, param.name, err.Error())
				}
			} else if param.IsRequired() {
				return NewError(paramsAPI.name, param.name, "missing formData param")
			}

		case "body":
			// Theoretically there should be at most one `body` param, and can not exist with `formData` at the same time
			body := reqCtx.PostBody()
			if body != nil {
				if err = paramsAPI.bodyDecodeFunc(value, body); err != nil {
					return NewError(paramsAPI.name, param.name, err.Error())
				}
			} else if param.IsRequired() {
				return NewError(paramsAPI.name, param.name, "missing body param")
			}

		case "header":
			paramValueBytes := reqCtx.Request.Header.Peek(param.name)
			if paramValueBytes != nil {
				if err = convertAssign(value, []string{string(paramValueBytes)}); err != nil {
					return NewError(paramsAPI.name, param.name, err.Error())
				}
			} else if param.IsRequired() {
				return NewError(paramsAPI.name, param.name, "missing header param")
			}

		case "cookie":
			bcookie := reqCtx.Request.Header.Cookie(param.name)
			if bcookie != nil {
				switch value.Type().String() {
				case fasthttpCookieTypeString:
					c := fasthttp.AcquireCookie()
					defer fasthttp.ReleaseCookie(c)
					if err = c.ParseBytes(bcookie); err != nil {
						return NewError(paramsAPI.name, param.name, err.Error())
					}
					value.Set(reflect.ValueOf(*c))

				case stringTypeString:
					value.Set(reflect.ValueOf(string(bcookie)))

				case bytesTypeString, bytes2TypeString:
					value.Set(reflect.ValueOf(bcookie))

				default:
					return NewError(paramsAPI.name, param.name, "invalid cookie param type, it must be `fasthttp.Cookie`, `string` or `[]byte`")
				}

			} else if param.IsRequired() {
				return NewError(paramsAPI.name, param.name, "missing cookie param")
			}
		}
		if err = param.validate(value); err != nil {
			return err
		}
	}
	return
}

// fasthttpFormValues returns all post data values with their keys
// multipart, formValues data, post arguments
func fasthttpFormValues(reqCtx *fasthttp.RequestCtx) map[string][]string {
	// first check if we have multipart formValues
	multipartForm, err := reqCtx.MultipartForm()
	if err == nil {
		//we have multipart formValues
		return multipartForm.Value
	}
	valuesAll := make(map[string][]string)
	// if no multipart and post arguments ( means normal formValues   )
	if reqCtx.PostArgs().Len() == 0 {
		return valuesAll // no found
	}
	reqCtx.PostArgs().VisitAll(func(k []byte, v []byte) {
		key := string(k)
		value := string(v)
		// for slices
		if valuesAll[key] != nil {
			valuesAll[key] = append(valuesAll[key], value)
		} else {
			valuesAll[key] = []string{value}
		}
	})
	return valuesAll
}
