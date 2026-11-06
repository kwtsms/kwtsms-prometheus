FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /kwtsms-prometheus .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /kwtsms-prometheus /kwtsms-prometheus
EXPOSE 9097
ENTRYPOINT ["/kwtsms-prometheus"]
