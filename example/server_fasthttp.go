package main

import (
	"encoding/json"
	// "mime/multipart"
	"github.com/valyala/fasthttp"
	"net/http"
)

type fasthttpTestApiware struct {
	Id        int      `param:"in(path),required,desc(ID),range(1:2)"`
	Num       float32  `param:"in(query),name(n),range(0.1:10.19)"`
	Title     string   `param:"in(query),nonzero"`
	Paragraph []string `param:"in(query),name(p),len(1:10)" regexp:"(^[\\w]*$)"`
	Cookie    int      `param:"in(cookie),name(apiwareid)"`
	// Picture   multipart.FileHeader `param:"in(formData),name(pic),maxmb(30)"`
}

func fasthttpTestHandler(ctx *fasthttp.RequestCtx) {
	// set cookies
	var c fasthttp.Cookie
	c.SetKey("apiwareid")
	c.SetValue("123")
	ctx.Response.Header.SetCookie(&c)

	// bind params
	params := new(fasthttpTestApiware)
	err := myApiware.FasthttpBind(params, ctx, pattern)
	b, _ := json.MarshalIndent(params, "", " ")

	if err != nil {
		ctx.SetStatusCode(http.StatusBadRequest)
		ctx.Write(append([]byte(err.Error()+"\n"), b...))
	} else {
		ctx.SetStatusCode(http.StatusOK)
		ctx.Write(b)
	}
}

func fasthttpServer(addr string) {
	// server
	fasthttp.ListenAndServe(addr, fasthttpTestHandler)
}
