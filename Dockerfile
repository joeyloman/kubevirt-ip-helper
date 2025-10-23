FROM docker.io/golang:1.25-alpine3.22 AS builder
RUN mkdir /src /deps
RUN apk update && apk add git build-base binutils-gold
WORKDIR /deps
ADD go.mod /deps
RUN go mod download
ADD / /src
WORKDIR /src
RUN go build -mod=vendor -o kubevirt-ip-helper .
FROM docker.io/alpine:3.22
RUN adduser -S -D -H -h /app kubevirt-ip-helper
USER kubevirt-ip-helper
COPY --from=builder /src/kubevirt-ip-helper /app/
WORKDIR /app
ENTRYPOINT ["./kubevirt-ip-helper"]