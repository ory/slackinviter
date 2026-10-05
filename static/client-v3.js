(function () {
  var form = document.getElementById("invite");
  var button = form.querySelector("button[type=submit]");
  var spinner = button.querySelector(".spinner");
  var status = document.getElementById("form-status");

  var requiredMessages = {
    email: "Enter your email.",
    fname: "Enter your first name.",
    lname: "Enter your last name.",
    coc: "Accept the Code of conduct to continue.",
  };

  var serverFieldErrors = {
    "Missing email": "email",
    "Missing first name": "fname",
    "Missing last name": "lname",
    "You need to accept the code of conduct": "coc",
  };

  var genericError = "We couldn't send your invite. Try again later.";

  function setLoading(loading) {
    button.disabled = loading;
    spinner.toggleAttribute("hidden", !loading);
    if (loading) {
      button.setAttribute("aria-busy", "true");
    } else {
      button.removeAttribute("aria-busy");
    }
  }

  function setFieldError(name, message) {
    var field = document.getElementById(name);
    var slot = document.getElementById(name + "-error");
    slot.textContent = message;
    field.setAttribute("aria-invalid", "true");
    field.setAttribute("aria-describedby", slot.id);
  }

  function clearFieldError(name) {
    var field = document.getElementById(name);
    document.getElementById(name + "-error").textContent = "";
    field.removeAttribute("aria-invalid");
    field.removeAttribute("aria-describedby");
  }

  function clearStatus() {
    status.textContent = "";
    status.removeAttribute("data-state");
  }

  function setStatus(state, message) {
    status.dataset.state = state;
    status.textContent = message;
  }

  function validate() {
    var invalid = [];
    Object.keys(requiredMessages).forEach(function (name) {
      var field = document.getElementById(name);
      var empty = field.type === "checkbox" ? !field.checked : !field.value.trim();
      if (empty) {
        setFieldError(name, requiredMessages[name]);
        invalid.push(field);
      } else if (field.validity.typeMismatch) {
        setFieldError(name, "Enter a valid email address.");
        invalid.push(field);
      } else {
        clearFieldError(name);
      }
    });
    if (invalid.length) {
      invalid[0].focus();
    }
    return invalid.length === 0;
  }

  function showServerError(body) {
    var name = serverFieldErrors[body];
    if (name) {
      setFieldError(name, requiredMessages[name]);
      document.getElementById(name).focus();
      return;
    }
    var isSlackError = /^Failed to invite to team: [a-z_]+$/.test(body);
    setStatus("error", isSlackError ? body : genericError);
    button.focus();
  }

  function showSuccess() {
    setStatus("success", "Check your email for your invite.");
    form.querySelectorAll("input").forEach(function (el) {
      el.disabled = true;
    });
    button.disabled = true;
  }

  function onFieldChange(event) {
    if (event.target.id && requiredMessages[event.target.id]) {
      clearFieldError(event.target.id);
    }
    clearStatus();
  }

  function resetForm() {
    form.querySelectorAll("input").forEach(function (el) {
      el.disabled = false;
    });
    setLoading(false);
    clearStatus();
  }

  resetForm();
  window.addEventListener("pageshow", function (event) {
    if (event.persisted) {
      resetForm();
    }
  });

  form.addEventListener("input", onFieldChange);
  form.addEventListener("change", onFieldChange);

  form.addEventListener("submit", function (event) {
    event.preventDefault();
    clearStatus();
    if (!validate()) {
      return;
    }
    setLoading(true);

    fetch(form.action, {
      method: "POST",
      body: new URLSearchParams(new FormData(form)),
    })
      .then(function (res) {
        if (res.ok) {
          setLoading(false);
          showSuccess();
          return;
        }
        return res.text().then(function (text) {
          setLoading(false);
          showServerError(text.trim());
        });
      })
      .catch(function () {
        setLoading(false);
        showServerError("");
      });
  });
})();
