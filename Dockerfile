FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /kwtsms-alertmanager .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /kwtsms-alertmanager /kwtsms-alertmanager
EXPOSE 9097
ENTRYPOINT ["/kwtsms-alertmanager"]
