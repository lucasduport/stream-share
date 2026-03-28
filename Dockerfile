FROM golang:1.17-alpine

RUN apk add ca-certificates

WORKDIR /go/src/github.com/lucasduport/stream-share
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o stream-share .

FROM alpine:3
COPY --from=0  /go/src/github.com/lucasduport/stream-share/stream-share /

ENTRYPOINT ["/stream-share"]
