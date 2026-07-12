function check(a,b){if(a!==b)throw a+" !== "+b;} var i=0;do{i++;}while(i<5);check(i,5);var j=10;do{j++;}while(false);check(j,11);console.log('PASS');
