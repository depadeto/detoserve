FROM golang:1.22-alpine AS build
RUN apk add --no-cache curl
RUN curl -LO "https://dl.k8s.io/release/$(curl -Ls https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl" \
    && chmod +x kubectl && mv kubectl /usr/local/bin/
WORKDIR /app
COPY go.mod ./
RUN go mod download || true
COPY . .
RUN CGO_ENABLED=0 go build -o detoserve-agent .

FROM alpine:3.19
RUN apk add --no-cache ca-certificates
COPY --from=build /usr/local/bin/kubectl /usr/local/bin/kubectl
COPY --from=build /app/detoserve-agent /detoserve-agent
ENTRYPOINT ["/detoserve-agent"]
