FROM golang:1.26-alpine@sha256:8ac98ca534ac3f51e1f420a1dd2c15e74c75cfa0f23f3ad27eb5d7236c349a0c AS build
WORKDIR /src
COPY go.mod .
COPY *.go .
RUN go test ./... && go vet ./... && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/quota-api .

FROM alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8
RUN addgroup -S -g 10001 quota && adduser -S -u 10001 -G quota quota
COPY --from=build /out/quota-api /usr/local/bin/quota-api
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/quota-api"]
