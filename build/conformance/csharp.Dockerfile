# The C# SDK's conformance client of Agones (test/sdk/csharp), unchanged.
FROM mcr.microsoft.com/dotnet/sdk:8.0 AS build
ARG agones=third_party/agones
WORKDIR /agones
COPY ${agones}/sdks/csharp sdks/csharp
COPY ${agones}/test/sdk/csharp test/sdk/csharp
RUN dotnet publish test/sdk/csharp/csharp.csproj --configuration Release --output /client

FROM mcr.microsoft.com/dotnet/runtime:8.0
COPY --from=build /client /client
ENTRYPOINT ["dotnet", "/client/csharp.dll"]
