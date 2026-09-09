import { isRouteErrorResponse, useNavigate, useRouteError } from "react-router";

// ErrorPage is the root error boundary for router-level errors. It shows
// a status code, the error message, and a link back to the dashboard.
export default function ErrorPage() {
  const error = useRouteError();
  const navigate = useNavigate();
  const status = isRouteErrorResponse(error) ? error.status : 500;
  let title = "Something went wrong";
  let friendlyMessage = "We encountered an unexpected issue. Please try again.";

  if (status === 401 || (error instanceof Error && error.name === "UnauthorizedError")) {
    title = "Authentication required";
    friendlyMessage = "Please log in first to continue.";
  } else if (status === 403) {
    title = "Access denied";
    friendlyMessage = "You do not have permission to view or access this page.";
  } else if (status === 404) {
    title = "Page not found";
    friendlyMessage = "The page or resource you were looking for could not be found.";
  } else if (status === 429) {
    title = "Too many requests";
    friendlyMessage = "Please wait a moment before trying again.";
  } else if (status >= 500) {
    title = "Server temporarily unavailable";
    friendlyMessage = "Our server is having trouble processing your request. Please try again shortly.";
  } else if (error instanceof Error && error.message) {
    friendlyMessage = error.message;
  }
  return (
    <main data-testid="error-page" className="flex items-center justify-center min-h-screen bg-navy p-4">
      <div className="bg-white rounded-lg shadow-lg p-10 max-w-[420px] w-full text-center">
        <p className="text-4xl font-bold text-navy mb-1" aria-hidden="true">{status}</p>
        <h1 className="text-xl font-semibold text-navy mb-2">{title}</h1>
        <p className="text-text-secondary mb-4">{friendlyMessage}</p>
        <p role="status" className="text-sm text-text-muted mb-6">
          Your session and data are safe. Nothing was changed.
        </p>
        <button
          type="button"
          onClick={() => {
            if (status === 401 || (error instanceof Error && error.name === "UnauthorizedError")) {
              navigate("/login");
            } else {
              navigate("/devices");
            }
          }}
          className="inline-flex items-center px-4 py-2 text-sm font-medium bg-red text-white rounded-md hover:bg-red-hover transition-colors"
        >
          {status === 401 || (error instanceof Error && error.name === "UnauthorizedError") ? "Go to Login" : "Try again"}
        </button>
      </div>
    </main>
  );
}