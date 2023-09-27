FROM golang:latest AS builder
RUN mkdir -p /go/src/github.com/mailgun/kafka-pixy
COPY . /go/src/github.com/mailgun/kafka-pixy
WORKDIR /go/src/github.com/mailgun/kafka-pixy
RUN apk add build-base
RUN go mod download 
RUN go build -v -o /go/bin/kafka-pixy

FROM alpine:latest
LABEL maintainer="Maxim Vladimirskiy <horkhe@gmail.com>"
COPY --from=builder /go/bin/kafka-pixy /usr/bin/kafka-pixy
COPY ./entrypoint.sh /entrypoint.sh
EXPOSE 19091 19092
ENTRYPOINT ["/entrypoint.sh"]
