FROM golang:1.7

RUN mkdir -p /go/src/app
COPY gohttp-master /go/src/app/gohttp
WORKDIR /go/src/app/gohttp

RUN go build && ./gohttp
