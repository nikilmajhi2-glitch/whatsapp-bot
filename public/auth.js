// --- Firebase Config ---
const firebaseConfig = {
    apiKey: "AIzaSyAvuTedi4hNLDTHwNt3tElmZZmwmxBC_zo",
    authDomain: "rupeedesk7.firebaseapp.com",
    projectId: "rupeedesk7",
    storageBucket: "rupeedesk7.firebasestorage.app",
    messagingSenderId: "1013963357851",
    appId: "1:1013963357851:android:eea4e2e566c2244aed503e",
};

// --- Firebase Imports ---
import { initializeApp } from "https://www.gstatic.com/firebasejs/10.12.2/firebase-app.js";
import { getAuth, createUserWithEmailAndPassword, signInWithEmailAndPassword, onAuthStateChanged }
from "https://www.gstatic.com/firebasejs/10.12.2/firebase-auth.js";

// --- Init Firebase ---
const app = initializeApp(firebaseConfig);
const auth = getAuth(app);

// --- DOM Elements ---
const loginForm = document.getElementById('login-form'),
    signupForm = document.getElementById('signup-form');
const loginToggle = document.getElementById('login-toggle'),
    signupToggle = document.getElementById('signup-toggle');
const errorMessage = document.getElementById('error-message');

// --- Device ID Logic ---
function getDeviceId() {
    let deviceId = localStorage.getItem('deviceGuid');
    if (!deviceId) {
        deviceId = crypto.randomUUID();
        localStorage.setItem('deviceGuid', deviceId);
    }
    return deviceId;
}
document.getElementById('device-id-display').textContent = `Device ID: ${getDeviceId()}`;

// ----------------------------------------------------
// 🔥 AUTO REDIRECT WHEN LOGGED IN
// ----------------------------------------------------
onAuthStateChanged(auth, async user => {
    if (user) {
        
        // Prevent redirect loop while on admin page
        if (window.location.pathname.includes("/admin/")) return;
        
        // Normal user → index.html
        window.location.href = "index.html";
    }
});

// ----------------------------------------------------
// 🔥 ADMIN LOGIN (NO FIREBASE)
// ----------------------------------------------------
async function tryAdminLogin(email, password) {
    const res = await fetch("/api/admin-login", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ email, password })
    });
    
    if (res.ok) {
        window.location.href = "/admin/admin.html";
        return true;
    }
    
    return false;
}

// ----------------------------------------------------
// 🔥 LOGIN BUTTON
// ----------------------------------------------------
document.getElementById('login-btn').addEventListener('click', async () => {
    const email = document.getElementById('login-email').value;
    const password = document.getElementById('login-password').value;
    
    // 1️⃣ ADMIN LOGIN CHECK FIRST
    if (email === "admin@rupeedesk.com") {
        const ok = await tryAdminLogin(email, password);
        if (!ok) {
            errorMessage.textContent = "Wrong admin password.";
        }
        return; // stop Firebase login
    }
    
    // 2️⃣ NORMAL USER → FIREBASE LOGIN
    signInWithEmailAndPassword(auth, email, password)
        .then(() => {
            window.location.href = "index.html";
        })
        .catch(err => {
            errorMessage.textContent = "Invalid email or password.";
            console.error(err);
        });
});

// ----------------------------------------------------
// 🔥 SIGNUP BUTTON
// ----------------------------------------------------
document.getElementById('signup-btn').addEventListener('click', () => {
    const email = document.getElementById('signup-email').value;
    const password = document.getElementById('signup-password').value;
    const referralCode = document.getElementById('referral-code').value.trim().toUpperCase();
    
    if (referralCode) {
        sessionStorage.setItem('referralCode', referralCode);
    } else {
        sessionStorage.removeItem('referralCode');
    }
    
    createUserWithEmailAndPassword(auth, email, password)
        .catch(error => {
            if (error.code === 'auth/email-already-in-use') {
                errorMessage.textContent = 'This email is already registered.';
            } else {
                errorMessage.textContent = 'Could not create account. Please try again.';
            }
            console.error(error);
        });
});

// ----------------------------------------------------
// 🔥 FORM SWITCHING
// ----------------------------------------------------
loginToggle.addEventListener('click', () => {
    loginForm.classList.add('active');
    signupForm.classList.remove('active');
    loginToggle.classList.add('active');
    signupToggle.classList.remove('active');
});

signupToggle.addEventListener('click', () => {
    signupForm.classList.add('active');
    loginForm.classList.remove('active');
    signupToggle.classList.add('active');
    loginToggle.classList.remove('active');
});