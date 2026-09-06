import { isRouteErrorResponse, useNavigate, useRouteError } from "react-router";

// ErrorPage is the root error boundary for router-level errors. It shows
// a status code, the error message, and a link back to the dashboard.
export default function ErrorPage() {
  const error = useRouteError();
  const navigate = useNavigate();
  const status = isRouteErrorResponse(error) ? error.status : 500;
  const message =
    isRouteErrorResponse(error)
      ? error.statusText
      : error instanceof Error
        ? error.message
        : "Unknown error";

  return (
    <main data-testid="error-page" className="flex items-center justify-center min-h-screen bg-navy p-4">
      <div className="bg-white rounded-lg shadow-lg p-10 max-w-[420px] w-full text-center">
        <p className="text-4xl font-bold text-navy mb-1" aria-hidden="true">{status}</p>
        <h1 className="text-xl font-semibold text-navy mb-2">Something went wrong</h1>
        <p className="text-text-secondary mb-4">{message}</p>
        <p role="status" className="text-sm text-text-muted mb-6">
          Your session and data are safe. Nothing was changed.
        </p>
        <button
          type="button"
          onClick={() => navigate("/")}
          className="inline-flex items-center px-4 py-2 text-sm font-medium bg-red text-white rounded-md hover:bg-red-hover transition-colors"
        >
          Try again
        </button>
      </div>
    </main>
  );
}