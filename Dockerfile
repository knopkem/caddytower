FROM golang:1.25-bookworm AS build

WORKDIR /src

COPY go.mod ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown

ENV CGO_ENABLED=0 GOOS=linux

RUN go build \
	-trimpath \
	-ldflags="-s -w -X caddytower/internal/version.Version=${VERSION} -X caddytower/internal/version.Commit=${COMMIT} -X caddytower/internal/version.Date=${DATE}" \
	-o /out/caddytower ./cmd/caddytower

RUN mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

ENV CADDYTOWER_DATA_DIR=/data

COPY --chown=nonroot:nonroot --from=build /out/caddytower /app/caddytower
COPY --chown=nonroot:nonroot --from=build /out/data /data

EXPOSE 8080

ENTRYPOINT ["/app/caddytower"]
