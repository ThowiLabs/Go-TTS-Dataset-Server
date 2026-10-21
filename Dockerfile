FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /dataset-server .
RUN mkdir -p /tmp/data && chown 65532:65532 /tmp/data

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /dataset-server /app/dataset-server
COPY --from=build --chown=65532:65532 /tmp/data /data
COPY --chown=65532:65532 --from=build /src/web/static/vendor/THIRD_PARTY.txt /app/THIRD_PARTY.txt
ENV DATA_DIR=/data LISTEN_ADDR=:8080
EXPOSE 8080
VOLUME ["/data"]
USER 65532:65532
ENTRYPOINT ["/app/dataset-server"]
