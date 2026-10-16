class Automations {
    constructor(doc) {
        this.doc = doc;
    }

    init = () => {
        const root = this.doc.querySelector("div.entity-automations-automations");

        if (!root) {
            return false;
        }

        root.querySelectorAll("button.run-button").forEach((button) => {
            button.addEventListener("click", () => this.run(button));
        });

        return true;
    }

    run = async (button) => {
        // It really does something: turns real things on and off. Ask first.
        if (!window.confirm("Run \"" + button.dataset.name + "\" now?")) {
            return;
        }

        const resultElement = button.parentElement.querySelector(".run-result");
        resultElement.textContent = "";
        button.disabled = true;

        try {
            const response = await fetch("/plugins/automations/run", {
                method: "POST",
                headers: {"Content-Type": "application/json"},
                body: JSON.stringify({ID: button.dataset.id}),
            });

            if (!response.ok) {
                // Say so on the page: a click that silently does nothing is worse than an error.
                console.error("Failed to run the automation:", response.status, await response.text());
                resultElement.textContent = "Could not run it (" + response.status + ")";
                button.disabled = false;
                return;
            }

            // Whether it ran or not, the page is stale now, and shows which.
            location.reload();
        } catch (error) {
            console.error("Failed to run the automation:", error);
            resultElement.textContent = "Could not run it: " + error;
            button.disabled = false;
        }
    }
}

new Automations(document).init();
