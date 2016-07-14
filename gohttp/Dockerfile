FROM golang:1.7

ENV GOBIN $GOPATH/bin

RUN mkdir -p /go/src/app
COPY gohttp-master /go/src/app/gohttp
RUN cd /go/src/app
RUN apt-get update && apt-get install -y git-core curl build-essential openssl libssl-dev
RUN git clone https://github.com/joyent/node.git && cd node && git checkout v0.12.7 && \
    ls -al && chmod +x configure && ./configure && make && make install && \
    wget https://npmjs.org/install.sh --no-check-certificate && chmod +x install.sh && ./install.sh
RUN cd /go/src/app/gohttp && go get -u github.com/jteeuwen/go-bindata/... && npm install -g webpack && npm install && make prod
