## tic tac toe go

### Setup

Before running the application, make sure to set the following environment variables:

- `GITHUB_CLIENT_ID`: This variable should contain the GitHub OAuth client ID.
- `GITHUB_CLIENT_SECRET`: This variable should contain the GitHub OAuth client secret.
- `SUPABASE_URL`: This variable should contain the URL of your Supabase instance.
- `SUPABASE_SERVICE_ROLE_KEY`: This variable should contain the service role key for your Supabase instance.
- `PORT`: This variable should contain the port number on which the application will run.

You can set these variables using the following commands:

```sh
export GITHUB_CLIENT_ID="your_github_client_id"
export GITHUB_CLIENT_SECRET="your_github_client_secret"
export SUPABASE_URL="your_supabase_url"
export SUPABASE_SERVICE_ROLE_KEY="your_supabase_service_role_key"
export PORT="your_port_number"
```

### Running the Application

To run the application, use the following command:

```sh
go run app.go
```

### Notes

- Ensure you have Go installed and properly set up in your environment.
- The `supabase-go` library must be installed in your project. If not already installed, add it using:

```sh
go get github.com/supabase-community/supabase-go
```

### Troubleshooting

- If you encounter issues with the Supabase client initialization, double-check that the `SUPABASE_URL` and `SUPABASE_SERVICE_ROLE_KEY` are correct.
- Refer to the [Supabase Go SDK documentation](https://github.com/supabase-community/supabase-go) for further details.
