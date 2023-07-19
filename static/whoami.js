async function whoami() {
  try {
    const sessionData = await getSession();
    createAndSubmitForm(sessionData);
  } catch (error) {
    console.error(
      "Error during session retrieval and form submission: ",
      error
    );
  }
}
async function getSession() {
  const url = "https://project.console.ory.sh/sessions/whoami";
  console.log("getting session data");
  try {
    const response = await fetch(url, {
      credentials: "include",
      mode: "cors",
    });
    if (response.ok) {
      const responseData = await response.json();
      return responseData;
    } else {
      throw new Error(
        "Failed to fetch Ory Network session: " + response.status
      );
    }
  } catch (error) {
    console.error(error.message);
    throw error;
  }
}
async function createAndSubmitForm(sessionData) {
  const form = document.createElement("form");
  form.method = "POST";
  form.action = "/invitation";
  form.style.display = "none";
  const input = document.createElement("input");
  input.type = "hidden";
  input.name = "sessionData";
  input.value = JSON.stringify(sessionData);
  form.appendChild(input);
  document.body.appendChild(form);
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const formData = new FormData(form);
    try {
      const response = await fetch(form.action, {
        method: form.method,
        body: formData,
      });
      if (response.ok) {
        const htmlContent = await response.text();
        document.body.innerHTML = htmlContent;
      } else {
        throw new Error("Failed to fetch HTML content: " + response.status);
      }
    } catch (error) {
      console.error("Error while fetching HTML content: ", error.message);
      throw error;
    }
  });
  form.submit();
}
document.addEventListener("DOMContentLoaded", () => {
  // Execute when the document has finished loading
  whoami();
});
