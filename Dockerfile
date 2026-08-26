# Build contract: reproducible Linux build of the orc binary.
# NOTE: the container runs LOCAL_UNSAFE-equivalent isolation only (no
# macOS sandbox-exec); use it as a build/CI image, not as the pilot boundary.
FROM golang:1.23-alpine AS build
RUN apk add --no-cache git make
WORKDIR /src
COPY go.mod ./
COPY . .
RUN go vet ./... && go build -o /out/orc ./cmd/orc

FROM alpine:3.20
RUN apk add --no-cache git go make ca-certificates
COPY --from=build /out/orc /usr/local/bin/orc
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["orc"]
CMD ["serve", "--addr", "0.0.0.0:8080", "--data", "/data"]
