# ---- Build stage ----
FROM golang:1 AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/gatelm ./cmd/gatelm/

# ---- Runtime stage ----
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/gatelm /gatelm

EXPOSE 18765

ENTRYPOINT ["/gatelm"]
