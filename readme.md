## tic tac toe go

### Setup

Before running the application, make sure to set the following environment variables:

- `SUPABASE_URL`: This variable should contain the URL of your Supabase instance.
- `SUPABASE_KEY`: This variable should contain the API key for your Supabase instance.

You can set these variables using the following commands:

```sh
export SUPABASE_URL="your_supabase_url"
export SUPABASE_KEY="your_supabase_key"
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

- If you encounter issues with the Supabase client initialization, double-check that the `SUPABASE_URL` and `SUPABASE_KEY` are correct.
- Refer to the [Supabase Go SDK documentation](https://github.com/supabase-community/supabase-go) for further details.
