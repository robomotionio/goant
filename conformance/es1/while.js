function check(a,b){if(a!==b&&!(a!==a&&b!==b))throw a+" !== "+b;} var i=0,s=0;while(i<5){s+=i;i++;}check(s,10);var n=0;while(false){n++;}check(n,0);console.log('PASS');
